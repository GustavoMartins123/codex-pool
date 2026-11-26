package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"codex-pool-proxy/internal/bruteforce"
	"codex-pool-proxy/internal/ctxstore"
	"codex-pool-proxy/internal/fsperm"
	"codex-pool-proxy/internal/pacing"
	"codex-pool-proxy/internal/pricing"
	"codex-pool-proxy/internal/requestlog"
	"codex-pool-proxy/internal/transport"
	"golang.org/x/net/http2"
)

type proxyServer struct {
	http    *http.Server
	handler *proxyHandler
	cancel  context.CancelFunc
	release []func()
}

func runProxy(shutdownCtx context.Context, cfg *config) error {
	handled, err := prepareProxyStartup(cfg)
	if err != nil || handled {
		return err
	}
	app, err := buildProxyServer(shutdownCtx, cfg)
	if err != nil {
		return err
	}
	defer app.close()
	if err := serveUntilShutdown(app.http, app.handler, shutdownCtx.Done(), cfg.shutdownGrace); err != nil {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

func (app *proxyServer) close() {
	app.cancel()
	for i := len(app.release) - 1; i >= 0; i-- {
		app.release[i]()
	}
}

func prepareProxyStartup(cfg *config) (bool, error) {
	duckPath := getenv("DUCKDB_PATH", "./data/usage.duckdb")
	cfg.duckPath = duckPath
	var restorer pairedRestorer
	if err := restorer.recoverPairedRestore(cfg.storePath, duckPath); err != nil {
		return false, fmt.Errorf("recover paired restore: %w", err)
	}
	if cfg.rotatePassportKey {
		count, err := rotatePassportKey(cfg.storePath, os.Getenv("POOL_AUTH_ENCRYPTION_KEY_OLD"), os.Getenv("POOL_AUTH_ENCRYPTION_KEY"))
		if err != nil {
			return false, fmt.Errorf("rotate Passport key: %w", err)
		}
		log.Printf("rotated %d sealed client token(s)", count)
		return true, nil
	}
	if err := validateSecretStrength(cfg); err != nil {
		return false, fmt.Errorf("insecure configuration: %w", err)
	}
	if cfg.backupDir != "" && cfg.restoreManifest != "" {
		return false, errors.New("choose only one of -backup-dir or -restore-manifest")
	}
	if cfg.backupDir != "" {
		manifest, err := createPairedBackup(cfg.storePath, duckPath, cfg.backupDir)
		if err != nil {
			return false, fmt.Errorf("create paired backup: %w", err)
		}
		log.Printf("paired backup created: %s", manifest)
		return true, nil
	}
	if cfg.restoreManifest != "" {
		if err := restorer.restorePairedBackup(cfg.restoreManifest, cfg.storePath, duckPath); err != nil {
			return false, fmt.Errorf("restore paired backup: %w", err)
		}
		log.Printf("paired backup restored from %s", cfg.restoreManifest)
		return true, nil
	}
	// Credential vault: build the store, migrate plaintext files, and fail
	// closed when encrypted files exist without a key. Must run before the
	// pool is loaded and before any provider refresh touches the files.
	if cfg.decryptCredentials {
		store, err := buildCredentialStore()
		if err != nil {
			return false, fmt.Errorf("credential vault: %w", err)
		}
		vault := credentialVault{store: store}
		count, err := vault.decryptCredentialFiles(cfg.poolDir)
		if err != nil {
			return false, fmt.Errorf("decrypt credentials: %w", err)
		}
		log.Printf("credential vault: decrypted %d file(s) back to plaintext in %s", count, cfg.poolDir)
		return true, nil
	}
	if cfg.checkCredentials {
		store, err := buildCredentialStore()
		if err != nil {
			return false, fmt.Errorf("credential vault: %w", err)
		}
		vault := credentialVault{store: store}
		stats, err := vault.verifyCredentialFiles(cfg.poolDir)
		if err != nil {
			return false, fmt.Errorf("credential check: %w", err)
		}
		for _, failure := range stats.Failures {
			log.Printf("credential check failed: %s", failure)
		}
		if len(stats.Failures) > 0 {
			return false, fmt.Errorf("credential check: %d/%d file(s) failed (wrong key, corrupted, or plaintext; rotate with POOL_CREDENTIAL_KEY_PREVIOUS, migrate by starting the server once, or restore from backup)", len(stats.Failures), stats.Checked)
		}
		if stats.Previous > 0 {
			return false, fmt.Errorf("credential check: %d/%d file(s) still decode only via the previous key version; restart the server once to finish rotation", stats.Previous, stats.Checked)
		}
		log.Printf("credential check: %d file(s) decode with the current key", stats.Checked)
		return true, nil
	}
	vault, err := newCredentialVault(cfg.poolDir)
	if err != nil {
		return false, fmt.Errorf("credential vault: %w", err)
	}
	cfg.vault = vault
	if problems := fsperm.Verify(cfg.poolDir); len(problems) > 0 {
		return false, fmt.Errorf("credential vault: %d path(s) holding upstream credentials are group/world accessible, refusing to start:\n  %s\nFix with: chmod 0600 <file> (or chown to the pool user), then restart.",
			len(problems), strings.Join(problems, "\n  "))
	}
	return false, nil
}

func buildProxyServer(parent context.Context, cfg *config) (_ *proxyServer, err error) {
	shutdownCtx, cancel := context.WithCancel(parent)
	app := &proxyServer{cancel: cancel}
	defer func() {
		if err != nil {
			app.close()
		}
	}()

	// Create provider registry
	codexProvider := NewCodexProviderWithRealtime(cfg.responsesBase, cfg.realtimeBase, cfg.whamBase, cfg.refreshBase)
	claudeProvider := NewClaudeProvider(cfg.claudeBase)
	geminiProvider := NewGeminiProvider(cfg.geminiBase, cfg.geminiAPIBase)
	antigravityProvider := NewAntigravityProvider(cfg.antigravityDailyBase, cfg.antigravityProdBase)
	antigravityRT := newAntigravityRuntime()
	antigravityProvider.runtime = antigravityRT
	kimiProvider := NewKimiProvider(cfg.kimiBase)
	minimaxProvider := NewMinimaxProvider(cfg.minimaxBase)
	zaiProvider := NewZAIProvider(cfg.zaiBase)
	xiaomiProvider := NewXiaomiProvider(cfg.xiaomiBase)
	grokProvider := NewGrokProvider(cfg.grokBase)
	adverserialProvider := NewAdverserialProvider(cfg.adverserialBase)
	opencodeGoProvider := NewOpencodeGoProvider(cfg.opencodeGoBase)
	qwenProvider := NewQwenProvider(cfg.qwenBase)
	aistudioProvider := NewAIStudioProvider(cfg.aistudioBase)
	genericProviders, genericAccounts, err := buildGenericProviders(cfg.genericProviders)
	if err != nil {
		return nil, fmt.Errorf("configure generic providers: %w", err)
	}
	federatedProviders, federatedAccounts, err := buildFederatedProviders(cfg.federation)
	if err != nil {
		return nil, fmt.Errorf("configure federation: %w", err)
	}
	extraProviders := []ProviderDriver{antigravityProvider, kimiProvider, minimaxProvider, zaiProvider, xiaomiProvider, grokProvider, adverserialProvider, opencodeGoProvider, qwenProvider, aistudioProvider}
	extraProviders = append(extraProviders, genericProviders...)
	extraProviders = append(extraProviders, federatedProviders...)
	registry := NewProviderRegistry(codexProvider, claudeProvider, geminiProvider, extraProviders...)

	log.Printf("loading pool from %s", cfg.poolDir)
	accounts, err := loadPool(cfg.poolDir, registry, cfg.vault)
	if err != nil {
		return nil, fmt.Errorf("load pool: %w", err)
	}
	accounts = append(accounts, genericAccounts...)
	accounts = append(accounts, federatedAccounts...)
	pool := newPoolState(accounts, cfg.debug.Load())
	pool.registry = registry
	pool.antigravity = antigravityRT
	pool.tierThreshold = cfg.tierThreshold
	pool.observeAccountStates(time.Now())
	pool.configureRouting(cfg.routing)
	codexCount := pool.countByType(AccountTypeCodex)
	claudeCount := pool.countByType(AccountTypeClaude)
	geminiCount := pool.countByType(AccountTypeGemini)
	antigravityCount := pool.countByType(AccountTypeAntigravity)
	kimiCount := pool.countByType(AccountTypeKimi)
	minimaxCount := pool.countByType(AccountTypeMinimax)
	zaiCount := pool.countByType(AccountTypeZAI)
	xiaomiCount := pool.countByType(AccountTypeXiaomi)
	grokCount := pool.countByType(AccountTypeGrok)
	adverserialCount := pool.countByType(AccountTypeAdverserial)
	opencodeGoCount := pool.countByType(AccountTypeOpencodeGo)
	if pool.count() == 0 {
		log.Printf("warning: loaded 0 accounts from %s", cfg.poolDir)
	}

	store, err := newUsageStore(cfg.storePath, cfg.retentionDays)
	if err != nil {
		return nil, fmt.Errorf("open usage store: %w", err)
	}
	app.release = append(app.release, func() { _ = store.Close() })
	if cfg.ipPrivacy {
		if err := store.wipeOriginRawIPs(); err != nil {
			log.Printf("privacy: failed to wipe stored raw IPs: %v", err)
		} else {
			log.Printf("privacy: raw client IP persistence disabled and stored values wiped")
		}
	}
	if cfg.originRetention > 0 {
		if err := store.pruneOriginMetadata(cfg.originRetention); err != nil {
			log.Printf("privacy: origin metadata prune failed: %v", err)
		}
		go func() {
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				if err := store.pruneOriginMetadata(cfg.originRetention); err != nil {
					log.Printf("privacy: origin metadata prune failed: %v", err)
				}
			}
		}()
	}
	reserveBytes := int64(64 << 20)
	if value := os.Getenv("ANALYTICS_EMERGENCY_RESERVE_BYTES"); value != "" {
		if parsed, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil && parsed > 0 {
			reserveBytes = parsed
		} else {
			return nil, errors.New("invalid ANALYTICS_EMERGENCY_RESERVE_BYTES: expected a positive integer")
		}
	}
	if err := store.configureAnalyticsReserve(cfg.storePath+".analytics-reserve", reserveBytes); err != nil {
		return nil, fmt.Errorf("allocate analytics emergency reserve: %w", err)
	}

	// Restore persisted usage totals from BoltDB
	if persisted, err := store.loadAllAccountUsage(); err == nil && len(persisted) > 0 {
		pool.mu.RLock()
		restored := 0
		for _, a := range pool.accounts {
			if usage, ok := persisted[a.ID]; ok {
				a.mu.Lock()
				a.Totals = usage
				a.mu.Unlock()
				restored++
			}
		}
		pool.mu.RUnlock()
		log.Printf("restored usage totals for %d/%d accounts from disk", restored, len(persisted))
	}

	standardTransport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second, // TCP keepalives to prevent NAT/router timeouts
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0, // Disable - we handle timeouts per-request based on streaming
		ExpectContinueTimeout: 5 * time.Second,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
	}
	_ = http2.ConfigureTransport(standardTransport)
	antigravityTransport := transport.CloneAntigravityHTTP11(standardTransport)

	// Anthropic traffic defaults to the Node.js 24.x uTLS profile used by Claude
	// Code. Other providers retain their existing transports.
	var upstream http.RoundTripper = transport.NewBunHybrid(standardTransport)
	switch strings.ToLower(os.Getenv("CODEX_TLS_FINGERPRINT")) {
	case "rustls":
		upstream = transport.NewRustlsHybrid(standardTransport)
		log.Printf("codex rustls/uTLS hybrid transport enabled")
	case "off", "go", "standard":
		upstream = standardTransport
		log.Printf("standard Go TLS transport enabled")
	case "bun":
		upstream = transport.NewBunHybrid(standardTransport)
		log.Printf("Node.js/uTLS hybrid transport enabled (Anthropic traffic only)")
	case "bun-all":
		upstream = transport.NewBun()
		log.Printf("Node.js/uTLS transport enabled (all traffic)")
	default:
		log.Printf("Node.js/uTLS hybrid transport enabled by default (Anthropic traffic only)")
	}

	// CODEX_ANTHROPIC_PROXY_URL routes Anthropic API traffic through a proxy
	// so it appears to originate from a different IP. Other traffic goes direct.
	if anthropicProxy := os.Getenv("CODEX_ANTHROPIC_PROXY_URL"); anthropicProxy != "" {
		proxyURL, err := url.Parse(anthropicProxy)
		if err != nil {
			return nil, fmt.Errorf("invalid CODEX_ANTHROPIC_PROXY_URL: %w", err)
		}
		proxyTransport := transport.NewBunWithProxy(proxyURL)
		upstream = transport.NewAnthropicHostProxy(proxyTransport, upstream)
		log.Printf("anthropic proxy enabled: %s", proxyURL.Host)
	}

	// Token refreshes must use the server's direct egress. Refresh is a
	// credential-maintenance operation, not provider traffic, and sending it
	// through the Anthropic/residential proxy makes transient proxy failures
	// look like account failures.
	refreshTransport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   10,
	}
	_ = http2.ConfigureTransport(refreshTransport)
	if cfg.refreshProxyURL != "" {
		log.Printf("ignoring refresh_proxy_url; token refreshes always use direct server egress")
	}

	// The retired pool_users.json store is read once for the final Passport
	// migration and never consulted again during runtime.
	var legacyPoolUsers *PoolUserStore
	if cfg.passport.jwtSecret != "" {
		var err error
		legacyPoolUsers, err = newPoolUserStore(cfg.passport.legacyUsersPath)
		if err != nil {
			log.Printf("warning: failed to load legacy pool users: %v", err)
		}
	}

	passport, passportErr := newPassportStore(store.db)
	if passportErr != nil {
		return nil, fmt.Errorf("failed to initialize Pool Passport: %w", passportErr)
	}
	if err := passport.retireLegacyCredentials(legacyPoolUsers); err != nil {
		return nil, fmt.Errorf("legacy credential retirement failed: %w", err)
	}
	if err := passport.sweepMinuteCounters(time.Now()); err != nil {
		return nil, fmt.Errorf("policy minute counter sweep failed: %w", err)
	}
	log.Printf("Pool Passport initialized (%d principals)", len(passport.principals))
	if err := passport.initializeAccountAuthority(pool.allAccounts()); err != nil {
		return nil, fmt.Errorf("initialize account ownership: %w", err)
	}
	pool.accountAuthority = passport
	experiments, err := newExperimentTracker(store.db, cfg.experiments)
	if err != nil {
		return nil, fmt.Errorf("initialize experiments: %w", err)
	}

	// Initialize pricing data
	prices, err := pricing.New()
	if err != nil {
		return nil, fmt.Errorf("initialize pricing: %w", err)
	}

	duckAnalytics, err := newDuckAnalytics(cfg.duckPath, store.db)
	if err != nil {
		return nil, fmt.Errorf("open DuckDB analytics: %w", err)
	}
	app.release = append(app.release, func() { _ = duckAnalytics.Close() })
	if err := duckAnalytics.migrateSQLiteCosts(shutdownCtx, cfg.storePath, os.Getenv("ANALYTICS_DB_PATH"), time.Now()); err != nil {
		return nil, fmt.Errorf("migrate cost history: %w", err)
	}
	log.Print("DuckDB analytics initialized")

	// Initialize request pacer from env var. Default to disabled so the proxy
	// does not add inter-turn latency unless an operator opts in.
	paceMs := 0
	if v := os.Getenv("CODEX_REQUEST_PACE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			paceMs = n
		} else {
			return nil, errors.New("invalid CODEX_REQUEST_PACE_MS: malformed or out-of-range integer")
		}
	}
	var pacer *pacing.Pacer
	if paceMs > 0 {
		pacer = pacing.New(time.Duration(paceMs) * time.Millisecond)
	}

	trafficShadow, err := newTrafficShadowRuntime(store.db, cfg.experiments.Traffic)
	if err != nil {
		return nil, fmt.Errorf("traffic shadow configuration rejected: %w", err)
	}
	h := &proxyHandler{
		passportConfig:       cfg.passport,
		cfg:                  cfg,
		transport:            upstream,
		antigravityTransport: antigravityTransport,
		refreshTransport:     refreshTransport,
		pool:                 pool,
		passport:             passport,
		registry:             registry,
		store:                store,
		analyticsStore:       duckAnalytics,
		duckAnalytics:        duckAnalytics,
		pricing:              prices,
		aliases:              newModelAliases(cfg.modelAliases),
		effortCap:            newEffortCap(cfg.maxReasoningEffortByUser, cfg.maxReasoningEffortByOrigin),
		bruteForce:           bruteforce.New(),
		accountLocks:         bruteforce.NewWithPolicy(accountLockMaxFailures, accountLockDuration),
		metrics:              newMetrics(),
		routeTraces:          newRouteTraceStore(2048),
		contextHandoff:       newConversationHandoffStore(),
		requestQueue:         newFairQueue(),
		poolAuto:             newPoolAutoOrchestrator(nil),
		experiments:          experiments,
		trafficShadow:        trafficShadow,
		recent:               newRecentErrors(50),
		startTime:            time.Now(),
		pacer:                pacer,
		largeReplayBodies:    make(chan struct{}, 1),
	}
	h.ipAccess.Install(cfg.ipAccess)
	h.contextHandoff.registry = registry
	h.poolAuto.registry = registry
	claudeProvider.identity = &h.claudeIdentity
	opencodeGoProvider.identity = &h.claudeIdentity
	app.release = append(app.release, h.bruteForce.Stop, h.accountLocks.Stop)
	if passport != nil && h.effortCap != nil {
		passport.mu.RLock()
		for _, pr := range passport.principals {
			if pr.MaxReasoningEffort != "" {
				h.effortCap.setDynamicUser(pr.ID, pr.MaxReasoningEffort)
				if pr.Username != "" {
					h.effortCap.setDynamicUser(pr.Username, pr.MaxReasoningEffort)
				}
			}
		}
		passport.mu.RUnlock()
	}
	if store != nil {
		contextStore, err := ctxstore.New(store.db)
		if err != nil {
			return nil, fmt.Errorf("initialize native context store: %w", err)
		}
		h.nativeContext = &nativeContext{store: contextStore, pool: pool, provider: registry.ForType(AccountTypeCodex), transport: h.transport, refresh: h.refreshAccountAfterAuthFailure}
	}
	if err := h.bruteForce.PersistBans(passportBanStore{passport: passport, namespace: "ip"}, time.Now()); err != nil {
		return nil, fmt.Errorf("restore brute-force bans: %w", err)
	}
	if err := h.accountLocks.PersistBans(passportBanStore{passport: passport, namespace: "account"}, time.Now()); err != nil {
		return nil, fmt.Errorf("restore account sign-in locks: %w", err)
	}
	h.startUsagePoller(shutdownCtx)
	if err := h.startSessionBroker(shutdownCtx); err != nil {
		return nil, fmt.Errorf("failed to start session broker: %w", err)
	}
	if getenv("PROXY_ENABLE_QUOTA_INTELLIGENCE", "1") != "0" {
		h.startQuotaIntelligenceRefresher(shutdownCtx)
	} else {
		log.Printf("quota intelligence disabled by PROXY_ENABLE_QUOTA_INTELLIGENCE")
	}
	startAntigravityVersionUpdater(shutdownCtx, antigravityRT)
	h.startAntigravityModelPoller(shutdownCtx)
	h.startProviderModelPoller(shutdownCtx)
	h.startGenericProviderHealthPoller(shutdownCtx)
	h.startFederationPoller(shutdownCtx)

	// Probe account UUIDs for Claude OAuth accounts that don't have one yet.
	go h.probeClaudeAccountUUIDs(shutdownCtx)

	h.startConversationCleanup(shutdownCtx)

	// Background cleanup for request pacer (every 5 minutes)
	if pacer != nil {
		pacer.StartCleanup(shutdownCtx)
	}

	// Start file watcher for hot-reload of pool directory and config.
	watcher, err := newPoolWatcher(cfg.poolDir, cfg.configPath, h)
	if err != nil {
		return nil, fmt.Errorf("failed to start file watcher: %w", err)
	}
	app.release = append(app.release, watcher.close)
	codexProvider.fingerprint.startUpdater(shutdownCtx)

	routes := &proxyRoutes{proxyHandler: h, runtime: requestlog.System()}
	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           routes.newHTTPApp(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       5 * time.Minute, // Keep connections alive for reuse
	}

	// Configure HTTP/2 with settings optimized for long-running streams.
	http2Srv := &http2.Server{
		MaxConcurrentStreams:         250,
		IdleTimeout:                  5 * time.Minute,
		MaxUploadBufferPerConnection: 1 << 20, // 1MB
		MaxUploadBufferPerStream:     1 << 20, // 1MB
		MaxReadFrameSize:             1 << 20, // 1MB
	}
	if err := http2.ConfigureServer(srv, http2Srv); err != nil {
		log.Printf("warning: failed to configure HTTP/2 server: %v", err)
	}

	if cfg.adminToken != "" {
		log.Printf("admin token configured (len=%d)", len(cfg.adminToken))
	} else {
		log.Printf("WARNING: no admin token configured")
	}
	log.Printf("codex-pool proxy listening on %s (codex=%d, claude=%d, gemini=%d, antigravity=%d, kimi=%d, minimax=%d, zai=%d, xiaomi=%d, grok=%d, adverserial=%d, opencode_go=%d, request_timeout=%v, stream_timeout=%v, stream_idle_timeout=%v, websocket_idle_timeout=%v, websocket_heartbeat_interval=%v, websocket_read_limit=%d)",
		cfg.listenAddr, codexCount, claudeCount, geminiCount, antigravityCount, kimiCount, minimaxCount, zaiCount, xiaomiCount, grokCount, adverserialCount, opencodeGoCount, cfg.requestTimeout, cfg.streamTimeout, cfg.streamIdleTimeout, cfg.websocketIdleTimeout, cfg.websocketHeartbeatInterval, cfg.websocketReadLimit)
	if cfg.claudeTraceDir != "" {
		log.Printf("claude traffic tracing enabled: dir=%s body_limit=%d include_secrets=%v", cfg.claudeTraceDir, cfg.claudeTraceBodyLimit, cfg.claudeTraceSecrets)
	}
	app.http = srv
	app.handler = h
	return app, nil
}

func serveUntilShutdown(srv *http.Server, h *proxyHandler, shutdown <-chan struct{}, grace time.Duration) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-shutdown:
	}

	registry := h.webSocketRegistry()
	registry.beginDrain()
	log.Printf("shutdown requested: draining %d websocket sessions for up to %s", registry.count(), grace)

	drainCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- srv.Shutdown(drainCtx)
	}()

	drained := registry.wait(drainCtx)
	if !drained {
		remaining := registry.count()
		log.Printf("shutdown grace expired with %d websocket sessions; sending service-restart closes", remaining)
		registry.forceCloseAll()
	}
	_ = srv.Close()

	select {
	case err := <-shutdownDone:
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-time.After(time.Second):
	}
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-time.After(time.Second):
	}
	log.Printf("shutdown complete")
	return nil
}
