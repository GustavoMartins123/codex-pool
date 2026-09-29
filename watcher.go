package main

import (
	"fmt"
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

	reloadMu     sync.Mutex
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
			w.Close()
			return nil, fmt.Errorf("cannot watch config directory %s: %w", filepath.Dir(configPath), err)
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
	if err := pw.reloadConfigCandidate(); err != nil {
		log.Printf("config reload rejected (previous settings retained): %v", err)
	}
}
func (pw *poolWatcher) reloadConfigCandidate() error {
	pw.reloadMu.Lock()
	defer pw.reloadMu.Unlock()
	cfg, err := loadConfigFile(pw.configPath)
	if err != nil {
		return err
	}
	if err := validateConfigEnvironment(); err != nil {
		return err
	}
	h := pw.handler
	if cfg.Experiments.Traffic.Enabled && (h.trafficShadow == nil || h.trafficShadow.db == nil || h.experiments == nil) {
		return fmt.Errorf("traffic shadow requires initialized experiments and a persistent budget database")
	}
	newDebug := getConfigBool("PROXY_DEBUG", cfg.Debug, false)
	threshold := getConfigFloat64("TIER_THRESHOLD", cfg.TierThreshold, 0.50)
	proxies := effectiveTrustedProxies(cfg.TrustedProxies)
	allow, deny := effectiveIPPolicy(cfg)
	proxyNets, trustAll, err := parseTrustedProxies(proxies)
	if err != nil {
		return err
	}
	allowNets, err := parseIPNetList("PROXY_IP_ALLOW", allow)
	if err != nil {
		return err
	}
	denyNets, err := parseIPNetList("PROXY_IP_DENY", deny)
	if err != nil {
		return err
	}
	// Every candidate is fully validated above. No state is changed on a
	// rejected reload; reload serialization prevents competing publications.
	if h.trafficShadow != nil {
		if err := h.trafficShadow.Update(cfg.Experiments.Traffic); err != nil {
			return err
		}
	}
	// Install prepared policies without a fallible step after publication.
	installTrustedProxies(proxyNets, trustAll)
	globalIPAccess.install(allowNets, denyNets)
	h.pool.configureRouting(cfg.Routing)
	h.pool.mu.Lock()
	h.pool.debug = newDebug
	h.pool.mu.Unlock()
	h.cfg.setHotReloadable(threshold, cfg.Routing, cfg.ClientPolicies, cfg.Experiments)
	if h.experiments != nil {
		h.experiments.Configure(cfg.Experiments)
	}
	if h.aliases != nil {
		h.aliases.reload(cfg.ModelAliases)
	}
	if h.effortCap != nil {
		h.effortCap.reload(cfg.MaxReasoningEffortByUser, cfg.MaxReasoningEffortByOrigin)
	}
	h.cfg.debug.Store(newDebug)
	log.Printf("config hot-reload complete (debug=%v, tier_threshold=%.2f)", newDebug, threshold)
	return nil
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
