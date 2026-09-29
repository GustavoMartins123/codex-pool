package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// poolWatcher watches the pool directory and config file for changes,
// triggering automatic reloads without requiring a server restart.
type poolWatcher struct {
	watcher    *fsnotify.Watcher
	poolDir    string
	configPath string
	handler    *proxyHandler

	mu           sync.Mutex
	debouncePool *time.Timer
	debounceCfg  *time.Timer
}

const watcherDebounce = 500 * time.Millisecond

func newPoolWatcher(poolDir, configPath string, handler *proxyHandler) (*poolWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	pw := &poolWatcher{
		watcher:    w,
		poolDir:    poolDir,
		configPath: configPath,
		handler:    handler,
	}

	// Watch pool directory for credential file changes.
	if poolDir != "" {
		if err := w.Add(poolDir); err != nil {
			w.Close()
			return nil, err
		}
		log.Printf("watching pool directory: %s", poolDir)
	}

	// Watch the config file's PARENT DIRECTORY rather than the file itself:
	// config saves are atomic (temp + rename), which supersedes any
	// file-level watch after the first replace. A directory watch survives
	// replacements, renames, and delete/recreate cycles.
	if configPath != "" {
		if err := w.Add(filepath.Dir(configPath)); err != nil {
			// Non-fatal — config may not exist yet.
			log.Printf("warning: cannot watch config directory %s: %v", filepath.Dir(configPath), err)
		} else {
			log.Printf("watching config directory: %s", filepath.Dir(configPath))
		}
	}

	// Register the provider directories that exist at startup. Directories
	// created later are picked up from Create events in handleEvent.
	if poolDir != "" {
		entries, readErr := os.ReadDir(poolDir)
		if readErr == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				pw.watchDirIfNew(filepath.Join(poolDir, entry.Name()))
			}
		}
	}

	go pw.loop()
	return pw, nil
}

// watchDirIfNew adds dir to the watch set unless it is already watched.
func (pw *poolWatcher) watchDirIfNew(dir string) {
	for _, existing := range pw.watcher.WatchList() {
		if pathsEqual(existing, dir) {
			return
		}
	}
	if err := pw.watcher.Add(dir); err != nil {
		log.Printf("warning: cannot watch provider directory %s: %v", dir, err)
	}
}

// unwatchDir drops a stale watch (directory removed or renamed away).
func (pw *poolWatcher) unwatchDir(dir string) {
	for _, existing := range pw.watcher.WatchList() {
		if pathsEqual(existing, dir) {
			_ = pw.watcher.Remove(dir)
			return
		}
	}
}

func (pw *poolWatcher) loop() {
	for {
		select {
		case event, ok := <-pw.watcher.Events:
			if !ok {
				return
			}
			// Ignore chmod-only events.
			if event.Op == fsnotify.Chmod {
				continue
			}
			pw.handleEvent(event)

		case err, ok := <-pw.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("watcher error: %v", err)
		}
	}
}

func (pw *poolWatcher) handleEvent(event fsnotify.Event) {
	// Atomic writers stage through *.tmp files and dotfiles in the same
	// directories; they never carry reload-worthy state.
	if strings.HasSuffix(event.Name, ".tmp") || strings.HasPrefix(filepath.Base(event.Name), ".") {
		return
	}
	pw.mu.Lock()
	defer pw.mu.Unlock()

	// Config events match by path (case-insensitively on Windows, where
	// ReadDirectoryChangesW reports the on-disk casing, which may differ
	// from the CONFIG_PATH the operator wrote).
	if pw.configPath != "" && pathsEqual(event.Name, pw.configPath) {
		if pw.debounceCfg != nil {
			pw.debounceCfg.Stop()
		}
		pw.debounceCfg = time.AfterFunc(watcherDebounce, pw.reloadConfig)
		return
	}

	// Everything else only matters when it happens inside the pool
	// directory: the config directory may be watched simultaneously and its
	// unrelated files must not trigger pool reloads.
	if pw.poolDir == "" || !pathWithin(pw.poolDir, event.Name) {
		return
	}

	switch {
	case event.Has(fsnotify.Create):
		// A provider directory created after startup must be watched from
		// now on; its credential files are otherwise invisible to reloads.
		if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
			pw.watchDirIfNew(event.Name)
		}
	case event.Has(fsnotify.Remove), event.Has(fsnotify.Rename):
		// The watch on a removed/renamed directory is stale (and on Linux a
		// rename keeps pointing at the old inode); drop it. Recreating the
		// directory re-arms the watch via the Create event.
		pw.unwatchDir(event.Name)
	}

	if pw.debouncePool != nil {
		pw.debouncePool.Stop()
	}
	pw.debouncePool = time.AfterFunc(watcherDebounce, pw.reloadPool)
}

func (pw *poolWatcher) reloadPool() {
	log.Printf("pool directory changed, reloading accounts")
	pw.handler.reloadAccounts()
	counts := map[AccountType]int{}
	pw.handler.pool.mu.RLock()
	for _, a := range pw.handler.pool.accounts {
		counts[a.Type]++
	}
	pw.handler.pool.mu.RUnlock()
	log.Printf("hot-reload complete: codex=%d claude=%d gemini=%d antigravity=%d kimi=%d minimax=%d zai=%d xiaomi=%d grok=%d adverserial=%d opencode_go=%d",
		counts[AccountTypeCodex], counts[AccountTypeClaude], counts[AccountTypeGemini],
		counts[AccountTypeAntigravity],
		counts[AccountTypeKimi], counts[AccountTypeMinimax], counts[AccountTypeZAI], counts[AccountTypeXiaomi], counts[AccountTypeGrok], counts[AccountTypeAdverserial], counts[AccountTypeOpencodeGo])
}

func (pw *poolWatcher) reloadConfig() {
	log.Printf("config file changed, reloading non-sensitive settings")
	cfg, err := loadConfigFile(pw.configPath)
	if err != nil {
		log.Printf("config reload failed: %v", err)
		return
	}
	if cfg == nil {
		return
	}
	if err := validateTrafficShadowConfig(cfg.Experiments.Traffic); err != nil {
		log.Printf("config reload rejected: %v", err)
		return
	}
	if cfg.Experiments.Traffic.Enabled && (pw.handler.trafficShadow == nil || pw.handler.trafficShadow.db == nil) {
		log.Printf("config reload rejected: traffic shadow requires a persistent budget database")
		return
	}

	// Only reload safe, non-sensitive fields.
	newDebug := getConfigBool("DEBUG", cfg.Debug, false)
	pw.handler.cfg.debug.Store(newDebug)
	threshold := getConfigFloat64("TIER_THRESHOLD", cfg.TierThreshold, 0.15)
	routing := pw.handler.cfg.hotRouting()
	if err := validateRoutingConfig(cfg.Routing); err != nil {
		log.Printf("routing config reload rejected: %v", err)
	} else {
		routing = cfg.Routing
		pw.handler.pool.configureRouting(routing)
		log.Printf("reloaded routing profiles (default=%s overrides=%d)", pw.handler.pool.defaultRoutingProfile(), len(cfg.Routing.Profiles))
	}
	pw.handler.pool.mu.Lock()
	pw.handler.pool.debug = newDebug
	pw.handler.pool.mu.Unlock()
	pw.handler.cfg.setHotReloadable(threshold, routing, cfg.ClientPolicies, cfg.Experiments)
	if pw.handler.experiments != nil {
		pw.handler.experiments.Configure(cfg.Experiments)
		if pw.handler.trafficShadow != nil {
			if err := pw.handler.trafficShadow.Update(cfg.Experiments.Traffic); err != nil {
				log.Printf("traffic shadow reload rejected: %v", err)
				return
			}
		}
	}

	// Reload model aliases (built-in defaults + optional config overrides).
	if pw.handler.aliases != nil {
		pw.handler.aliases.reload(cfg.ModelAliases)
		log.Printf("reloaded model aliases (config overrides=%d)", len(cfg.ModelAliases))
	}
	if len(cfg.TrustedProxies) > 0 && os.Getenv("PROXY_TRUSTED_PROXIES") == "" && os.Getenv("TRUSTED_PROXIES") == "" {
		setTrustedProxies(cfg.TrustedProxies)
		log.Printf("reloaded trusted proxies (%d entries)", len(cfg.TrustedProxies))
	}

	// Reasoning-effort caps are operational throttles, so they retune without
	// a restart like aliases do.
	if pw.handler.effortCap != nil {
		pw.handler.effortCap.reload(cfg.MaxReasoningEffortByUser, cfg.MaxReasoningEffortByOrigin)
		log.Printf("reloaded reasoning effort caps (users=%d origins=%d)",
			len(cfg.MaxReasoningEffortByUser), len(cfg.MaxReasoningEffortByOrigin))
	}

	log.Printf("config hot-reload complete (debug=%v, tier_threshold=%.2f)",
		newDebug, pw.handler.cfg.hotTierThreshold())
}

func (pw *poolWatcher) close() {
	pw.mu.Lock()
	if pw.debouncePool != nil {
		pw.debouncePool.Stop()
	}
	if pw.debounceCfg != nil {
		pw.debounceCfg.Stop()
	}
	pw.mu.Unlock()
	pw.watcher.Close()
}
