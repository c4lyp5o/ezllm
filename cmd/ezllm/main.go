// Command ezllm is Calypso's lightweight LLM router.
//
// M2: config + SQLite store (WAL, single writer) +
// three passthrough surfaces (OpenAI chat, Anthropic messages, OpenAI
// Responses) with normalized usage ledgering, and graceful shutdown.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/c4lyp5o/ezllm/internal/codexbridge"
	"github.com/c4lyp5o/ezllm/internal/config"
	"github.com/c4lyp5o/ezllm/internal/logx"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/ratelimit"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/rules"
	"github.com/c4lyp5o/ezllm/internal/server"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func main() {
	// The codex bridge re-execs this binary as its Landlock helper: it confines
	// itself and execs the Codex CLI. It never returns.
	if len(os.Args) > 1 && os.Args[1] == codexbridge.LandlockHelperArg {
		codexbridge.LandlockMain(os.Args[2:])
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ezllm:", err)
		os.Exit(1)
	}
}

// announceLogging states the level and format the process actually runs with.
// A typo'd or ignored value is silent otherwise — the operator sees fewer lines
// and cannot tell a quiet gateway from a switch that did not take.
//
// The key is log_level, not level: slog already emits "level" for the record
// itself, and a second "level" is a duplicate JSON key whose value depends on
// which parser reads it.
func announceLogging(log *slog.Logger, level slog.Level, jsonFormat, color bool) {
	format := "console"
	if jsonFormat {
		format = "json"
	}
	log.Info("console logging", "log_level", logx.LevelName(level), "format", format, "color", color)
}

func run() error {
	// Subcommands run before flag parsing (they own the whole process).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "github-login":
			return runGitHubLogin()
		case "help", "-h", "--help":
			fmt.Println("usage: ezllm [flags]            start the router")
			fmt.Println("       ezllm github-login       authorize a GitHub Copilot account (device flow)")
			return nil
		}
	}

	cfgPath := flag.String("config", "config.yaml", "path to config file")
	addrOverride := flag.String("addr", "", "listen address override (precedence: -addr > EZLLM_ADDR > config.listen)")
	flag.Parse()

	// Log level and color: env-only, because they have to work before the
	// config is readable (and are container-friendly). EZLLM_LOG_COLOR is
	// auto|always|never; auto colors a terminal only and honors NO_COLOR.
	rawLevel := os.Getenv("EZLLM_LOG_LEVEL")
	level, levelOK := logx.ParseLevel(rawLevel)
	color := logx.ColorEnabled(os.Getenv("EZLLM_LOG_COLOR"), os.Stdout)
	jsonFormat := logx.ParseFormat(os.Getenv("EZLLM_LOG_FORMAT"))
	log := slog.New(logx.NewHandler(os.Stdout, logx.Options{Level: level, Color: color, JSON: jsonFormat}))
	slog.SetDefault(log)
	if !levelOK {
		log.Warn("unknown EZLLM_LOG_LEVEL, using info", "value", rawLevel, "accepted", "debug|info|warn|error")
	}
	// Say what the console will actually do. A typo'd or ignored level is
	// silent otherwise: the operator sees fewer lines and cannot tell whether
	// the gateway is quiet or the switch did not take.
	announceLogging(log, level, jsonFormat, color)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	// Precedence: flag > EZLLM_* env > config.yaml > built-in default. The two
	// env overrides exist because the shipped defaults are repo-relative
	// (`data`) and loopback-only — both wrong inside a container, where the
	// Dockerfile sets EZLLM_DATA_DIR=/data and EZLLM_ADDR=0.0.0.0:20129.
	if v := strings.TrimSpace(os.Getenv("EZLLM_DATA_DIR")); v != "" {
		cfg.DataDir = v
	}
	// Translation DEFAULTS ON (config.go seeds it); EZLLM_TRANSLATION_ENABLED
	// is the no-rebuild rollback lever for the ONE switch (translationFromEnv).
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}
	if w, err := translationFromEnv(&cfg.Translation, os.Getenv); err != nil {
		return err
	} else if w != "" {
		log.Warn(w)
	}

	// ── master key (at-rest encryption of provider keys) ────────────────
	masterSecret := strings.TrimSpace(os.Getenv("EZLLM_MASTER_KEY"))
	if masterSecret == "" {
		masterPath := strings.TrimSpace(os.Getenv("EZLLM_MASTER_KEY_FILE"))
		if masterPath == "" {
			home, herr := os.UserHomeDir()
			if herr != nil || home == "" || home == "/" {
				// Containers often have no writable home; keep the key with the
				// data volume so it is backed up with the DB.
				home = cfg.DataDir
			}
			masterPath = filepath.Join(home, ".hermes", "secrets", "ezllm-master.key")
		}
		var created bool
		masterSecret, created, err = loadOrCreateMasterKey(masterPath)
		if err != nil {
			return fmt.Errorf("master key: %w", err)
		}
		if created {
			log.Info("generated new master key", "path", masterPath, "action", "back up with the DB")
		}

	}

	// ── data dir + store ──
	dbPath := filepath.Join(cfg.DataDir, "ezllm.sqlite")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	openCtx, cancelOpen := context.WithTimeout(ctx, 30*time.Second)
	defer cancelOpen()

	db, err := store.Open(openCtx, store.Options{
		Path:         dbPath,
		BatchSize:    cfg.Ledger.BatchSize,
		BatchWait:    cfg.LedgerBatchWait,
		MasterSecret: masterSecret,
	})
	if err != nil {
		return err
	}
	defer db.Close()
	log.Info("store ready", "path", dbPath, "schema", db.SchemaVersion())

	// First boot: no dashboard password exists. Seed it from the env, or
	// mint the one-time setup code the set-password page asks for.
	setupCode, err := prepareDashboardSetup(ctx, db, log)
	if err != nil {
		return err
	}
	// Reverse proxies whose X-Forwarded-For the sign-in rate limiter
	// believes. Unset = socket peer only (correct without a proxy; behind one,
	// every client would share the proxy's bucket).
	trustedProxies, err := server.ParseTrustedProxies(os.Getenv("EZLLM_TRUSTED_PROXIES"))
	if err != nil {
		return fmt.Errorf("EZLLM_TRUSTED_PROXIES: %w", err)
	}
	if len(trustedProxies) > 0 {
		log.Info("trusted proxies for sign-in rate limiting", "proxies", os.Getenv("EZLLM_TRUSTED_PROXIES"))
	}

	// ── wiring ──
	client := upstreamClient(cfg)
	addr := cfg.Listen
	if env := strings.TrimSpace(os.Getenv("EZLLM_ADDR")); env != "" {
		addr = env
	}
	if *addrOverride != "" {
		addr = *addrOverride
	}

	registry := provider.NewRegistry(client)
	// gemini-web and claude-web hops are rewritten to their in-process
	// bridges, served on this server's own listener. NewRegistry assumes the
	// default listen address; the real one is known only here.
	origin := bridgeOrigin(addr)
	registry.Register(provider.NewGeminiWeb(origin+"/internal/geminiweb/v1", client))
	registry.Register(provider.NewClaudeWeb(origin+"/internal/claudeweb/v1", client))

	// Seed accounts from config (idempotent; config is bootstrap only —
	// everything after M3 is managed through the admin API).
	if err := seed(openCtx, db, cfg, registry, log); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	dispatcher := proxy.NewDispatcher(client, registry)
	dispatcher.SetRetries(cfg.Retry.Retries)
	// Request-wide attempt budget (addendum #3): retries + failovers counted
	// together, so one client request cannot hammer N hops x retries.
	dispatcher.SetAttemptBudget(cfg.Retry.RequestAttemptBudget)
	if cfg.Retry.DispatchDeadlineStr != "" {
		if d, err := time.ParseDuration(cfg.Retry.DispatchDeadlineStr); err == nil {
			dispatcher.SetDispatchDeadline(d)
		}
	}
	// Failover diagnostics go to the same logger as the request log: a hop that
	// was skipped (parked) or failed over writes no ledger row, so without this
	// the reason a combo silently stopped using a hop exists nowhere.
	dispatcher.SetLogger(log)
	// cooldown.on_429 / cooldown.on_5xx / cooldown.max_per_key were parsed and
	// validated but read by nothing: the dispatcher used one hardcoded
	// five-minute park for every failure class. Wire the operator's numbers
	// through so the config means what it says.
	dispatcher.ConfigureCooldown(cfg.CooldownOn429, cfg.CooldownOn5xx, cfg.Cooldown.MaxPerKey)

	// ── account request-budget gate ──
	// In-memory by design: a reservation runs on EVERY request, and the store's
	// single write connection is already the busiest thing in the process. A
	// DB-backed reservation measured ~8k/s with millisecond tails once 30+
	// clients were in flight; this measures ~3M/s at sub-microsecond median.
	//
	// The ledger stays the source of truth — the window is seeded from it here,
	// so a restart does not hand a spent credential a fresh budget at exactly
	// the moment a reconnect burst is likely.
	limiter := ratelimit.New(nil)
	seedRateLimiter(openCtx, db, limiter, log)
	dispatcher.SetLimiter(limiter)
	resolver := router.New(db)

	// ── M5 rules engine ──
	// The eligibility predicate behind router's WithEligibility seam: token
	// caps and allowed-hours windows. Built once, refreshed on a ticker and
	// after any admin write (the server calls Engine.Reload).
	// A failure to load rules at boot must not abort startup: routing with no
	// rules is the M2 behaviour (allow everything), which is safe, whereas a
	// crash here would take the gateway down over a transient store error. So
	// on a load failure we log it and fall back to a permissive engine — the
	// background ticker keeps retrying via Reload, so rules reappear once the
	// store recovers.
	ruleEngine, err := rules.NewEngineRetrying(
		storeAdapter{db}, storeAdapter{db}, rules.SystemClock, 30*time.Second)
	if err != nil {
		log.Warn("rules engine failed to load at boot; running unrestricted (ticker will retry)", "err", err)
	}
	resolver = resolver.WithEligibility(ruleEngine.Eligible)
	// Translation of anthropic requests onto openai-only hops. Opt-in at
	// boot; a set-but-unread knob must never look identical to a working
	// one, so the effective state is logged here.
	resolver = resolver.WithTranslation(cfg.Translation.Enabled)
	// Curated responses-wire hops ride /v1/responses for chat AND anthropic
	// clients (tokenrouter gpt-6.1-sol refuses tools+reasoning on chat and has
	// no /v1/messages; Claude Code reaches it with the anthropic→responses
	// translator). The list itself is the opt-in; empty list = inert. Logged at
	// boot because a set-but-unread knob looks identical to a working one.
	resolver = resolver.WithResponsesModels(cfg.Translation.ResponsesModels)
	// The universal matrix: three NEW cross-wire cells plus the fixed
	// fallback ordering. Logged at boot because a set-but-unread knob looks
	// identical to a working one.
	// translation.enabled covers it too: one switch for every translated wire.
	resolver = resolver.WithUniversal(cfg.Translation.Enabled)
	log.Info("translation", "enabled", cfg.Translation.Enabled,
		"responses_wire_models", cfg.Translation.ResponsesModels)

	// EZLLM_USAGE_TZ sets the default zone for group_by=day bucketing so the
	// dashboard's day view agrees with a usage rule's day. Validated HERE,
	// at boot, with the real resolver — not with a regex. A typo must be a
	// startup error: the C5 lesson is that a tz which fails to resolve and
	// falls back silently is an 8-hour window shift with nothing pointing at
	// it, and the operator just sees a rule that "is not working".
	//
	// Per-request ?tz= still overrides it; this is only the default.
	usageTZ := strings.TrimSpace(os.Getenv("EZLLM_USAGE_TZ"))
	usageLoc := time.UTC
	if usageTZ != "" {
		loc, err := time.LoadLocation(usageTZ)
		if err != nil {
			return fmt.Errorf("EZLLM_USAGE_TZ=%q does not resolve to a time zone on this host "+
				"(use an IANA name like Asia/Kuala_Lumpur, or unset it for UTC): %w", usageTZ, err)
		}
		usageLoc = loc
		log.Info("usage day bucketing zone", "tz", usageTZ)
	}

	srv := server.New(server.Options{
		Auth: db, Resolver: resolver, Dispatcher: dispatcher, DB: db,
		Registry: registry, Log: log, Rules: ruleEngine,
		MaxBodyMiB:  cfg.MaxBodyMiB,
		CORSOrigins: nil,
		UsageTZ:     usageTZ,
		// Compression policy (addendum #5): absolute floor + CPU cap.
		CompressionMinAbs: cfg.Compression.MinAbsoluteTokens,
		CompressionMaxMS:  cfg.Compression.MaxPipelineMS,
		// Sampled shadow compression (addendum #7): off unless configured.
		ShadowSampler: shadowSamplerFor(cfg.Compression.ShadowPct),
		// A combo write probes only its own untested hops — never a catalog.
		ComboAutoProbe: os.Getenv("EZLLM_COMBO_AUTOPROBE") != "off",
		SetupCode:      setupCode,
		TrustedProxies: trustedProxies,
	})

	// Catalogs at boot and every model_refresh.interval (default 12h); each
	// saved catalog resyncs the auto-free combos and probes its free models.
	refreshEvery, _ := time.ParseDuration(cfg.ModelRefresh.Interval) // validated by config.Load
	srv.StartModelRefresh(ctx, refreshEvery, 30*time.Second)

	if os.Getenv("EZLLM_PRICE_REFRESH") != "off" {
		srv.StartPriceRefresh(ctx, 3, usageLoc)
	}

	// Claude subscription surface: loopback-only, off unless EZLLM_CLAUDE_BRIDGE=on.
	// Enabling it without a usable CLI or login fails the boot loudly.
	stopClaude, err := startClaudeBridge(ctx, log, srv.EnableBridge)
	if err != nil {
		return err
	}
	// ChatGPT subscription surface: loopback-only, off unless EZLLM_CODEX_BRIDGE=on.
	stopCodex, err := startCodexBridge(ctx, log, srv.EnableBridge)
	if err != nil {
		stopClaude()
		return err
	}

	reqs := newRequestLifetime()
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           reqs.track(srv.Handler()),
		BaseContext:       reqs.baseContext,
		ReadHeaderTimeout: 10 * time.Second,
		// Idle keep-alive connections are closed after 2 minutes. Request
		// bodies get their own read budget per handler (server.limitBodyRead).
		IdleTimeout: 2 * time.Minute,
		// No WriteTimeout: SSE streams are unbounded by design (M7 tunes the rest).
	}

	if setupCode != "" {
		url := bridgeOrigin(addr)
		logx.FirstBootBanner(os.Stdout, color, url, setupCode)
		log.Warn("dashboard setup pending: open the dashboard and enter the setup code above", "url", url)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "surfaces", "chat,anthropic,responses")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down", "drain_max", "15s")
	// The store closes only after every handler has returned (or the abort
	// wait expires), so in-flight requests still get their ledger rows.
	reqs.drain(httpSrv, 15*time.Second, 5*time.Second, log)
	// Bridges after the gateway (its handlers proxy into them), joined here
	// so no CLI child outlives the process's own shutdown.
	stopCodex()
	stopClaude()
	// Background probes and refresh loops write to the store: cancel them
	// and wait before it closes.
	if !srv.StopBackground(10 * time.Second) {
		log.Error("background jobs still running at store close; their writes may fail")
	}
	// Stop the rules-engine ticker first so it cannot reload against a closing
	// store, then db.Close() drains the ledger queue and flushes the pools.
	ruleEngine.Close()
	if err := db.Close(); err != nil {
		return fmt.Errorf("store close: %w", err)
	}
	// Rows from handlers that outlived the abort wait; nonzero = lost usage.
	if n := store.DroppedAfterClose(); n > 0 {
		log.Error("ledger rows recorded after store close were dropped", "rows", n)
	}
	log.Info("stopped")
	return nil
}

// bridgeOrigin is the URL this process dials to reach its OWN listener. A
// wildcard bind (":port", 0.0.0.0, [::]) is not a dialable address on every
// platform; loopback always is, in-process.
func bridgeOrigin(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// seedRateLimiter preloads each budgeted account's recent requests from the
// ledger into the in-memory window.
//
// Best-effort by design: a failure logs and leaves that account's budget empty,
// which at worst lets one extra window of requests through after a restart — the
// provider's own 429 is still the backstop. Aborting boot over it would take the
// gateway down to avoid a far smaller problem.
func seedRateLimiter(ctx context.Context, db *store.DB, l *ratelimit.Limiter, log *slog.Logger) {
	rates, err := db.AccountsWithRateLimit(ctx)
	if err != nil {
		log.Warn("rate limiter seed: could not read accounts", "err", err)
		return
	}
	if len(rates) == 0 {
		return
	}
	now := time.Now()
	for _, r := range rates {
		times, err := db.CallTimesSince(ctx, r.AccountID, now.Add(-r.Window))
		if err != nil {
			log.Warn("rate limiter seed: could not read calls", "account_id", r.AccountID, "err", err)
			continue
		}
		l.Seed(r.AccountID, times)
		log.Info("rate limiter seeded", "account_id", r.AccountID,
			"limit", r.Limit, "window", r.Window.String(), "in_window", len(times))
	}
}

// Provider API keys are entered through the dashboard, never environment variables.
func seed(ctx context.Context, db *store.DB, cfg *config.Config, registry *provider.Registry, log *slog.Logger) error {
	for name, p := range cfg.Providers {
		kind, err := provider.ParseKind(p.Kind)
		if err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}
		ns := p.Namespace
		if ns == "" {
			ns = name
		}
		acct := provider.Account{
			Name: name, Namespace: ns, Kind: kind, BaseURL: p.BaseURL,
			Enabled:               true,
			RequiresSessionHeader: p.RequiresSession(),
			ProbeDelay:            time.Duration(p.ProbeDelayMs) * time.Millisecond,
			CustomHeaders:         p.CustomHeaders,
			QuotaMode:             p.QuotaMode,
		}
		id, err := db.UpsertAccount(ctx, acct)
		if err != nil {
			return fmt.Errorf("account %q: %w", name, err)
		}
		// Sync the catalog so /v1/models and namespace resolution work.
		// NOTE: pass the PERSISTED id — the local struct's ID is zero here, and
		// PickKey(0) fails with "store: not found" (this exact bug hid the
		// catalog at boot).
		acct.ID = id
		if models, err := syncModels(ctx, db, registry, acct); err != nil {
			log.Warn("model sync failed at boot (routing still works; sync later via admin)",
				"provider", name, "err", err)
		} else {
			log.Info("synced model catalog", "provider", name, "models", len(models))
		}
	}

	// Client tokens and provider credentials are managed in SQLite through admin APIs.
	return nil
}

// syncModels fetches and stores an account's catalog. Failures are non-fatal at
// boot (an empty catalog still routes — the upstream is authoritative).
func syncModels(ctx context.Context, db *store.DB, registry *provider.Registry, acct provider.Account) ([]provider.ModelInfo, error) {
	ad, err := registry.Get(acct.Kind)
	if err != nil {
		return nil, err
	}
	key, err := db.PickKey(ctx, acct.ID)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	models, err := ad.ListModels(sctx, acct, key.Plaintext)
	if err != nil {
		return nil, err
	}
	if err := db.UpsertModels(ctx, acct.ID, models); err != nil {
		return models, err
	}
	return models, nil
}

// shadowSamplerFor arms the discarded shadow-compression pass (addendum #7)
// for the configured percentage of requests. 0 = nil = never runs.
func shadowSamplerFor(pct int) func() bool {
	if pct <= 0 {
		return nil
	}
	if pct >= 100 {
		return func() bool { return true }
	}
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(pct)))
	var mu sync.Mutex
	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		return rng.IntN(100) < pct
	}
}

// upstreamClient builds the HTTP client for provider calls. No client-level
// Timeout by default: streaming responses are unbounded, and per-request
// deadlines come from the caller's context. Redirects stay on the origin the
// request was sent to: provider keys and prompts never follow one elsewhere.
func upstreamClient(cfg *config.Config) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: cfg.UpstreamTimeout, CheckRedirect: provider.SameOriginRedirect}
}

// translationFromEnv applies the env override of the ONE translation switch.
// EZLLM_TRANSLATION_ENABLED wins; the deprecated
// EZLLM_TRANSLATION_UNIVERSAL_ENABLED still applies when it is the only one
// set, so an old rollback keeps working, and returns a deprecation warning
// either way. A malformed value is a startup error: silently defaulting an
// "off" to "on" would leave the operator wondering why traffic still
// translates.
func translationFromEnv(t *config.Translation, getenv func(string) string) (string, error) {
	parse := func(name string) (bool, bool, error) {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return false, false, nil
		}
		on, err := strconv.ParseBool(v)
		if err != nil {
			return false, false, fmt.Errorf("%s=%q is not a boolean (want true/false/1/0): %w", name, v, err)
		}
		return on, true, nil
	}
	on, set, err := parse("EZLLM_TRANSLATION_ENABLED")
	if err != nil {
		return "", err
	}
	legacyOn, legacySet, err := parse("EZLLM_TRANSLATION_UNIVERSAL_ENABLED")
	if err != nil {
		return "", err
	}
	warning := ""
	if legacySet {
		warning = "EZLLM_TRANSLATION_UNIVERSAL_ENABLED is deprecated: EZLLM_TRANSLATION_ENABLED is the one switch now"
		if !set {
			on, set = legacyOn, true
		}
	}
	if set {
		t.Enabled, t.UniversalEnabled = on, on
	}
	return warning, nil
}
