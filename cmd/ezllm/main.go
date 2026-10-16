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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/c4lyp5o/ezllm/internal/config"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/ratelimit"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/rules"
	"github.com/c4lyp5o/ezllm/internal/server"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ezllm:", err)
		os.Exit(1)
	}
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

	// Log level: EZLLM_LOG_LEVEL is the only knob that has to work before the
	// config is readable, so it is env-only (and container-friendly).
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("EZLLM_LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

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
	// Translation DEFAULTS ON (config.go seeds it); this env var is the
	// override — the no-rebuild rollback lever. Eligibility still needs a hop
	// proven/curated openai-only, so the knob alone changes no routing. A
	// malformed value is a startup error: silently defaulting an "off" to "on"
	// would leave the operator wondering why traffic still translates.
	if v := strings.TrimSpace(os.Getenv("EZLLM_TRANSLATION_ENABLED")); v != "" {
		on, perr := strconv.ParseBool(v)
		if perr != nil {
			return fmt.Errorf("EZLLM_TRANSLATION_ENABLED=%q is not a boolean (want true/false/1/0): %w", v, perr)
		}
		cfg.Translation.Enabled = on
	}
	// Universal wire matrix: default OFF, opt-in at config or via this env
	// (the no-rebuild rollback lever). A malformed value is a startup error,
	// never a silent default: "off" turning "on" would activate three new
	// cross-wire cells the operator did not ask for.
	if v := strings.TrimSpace(os.Getenv("EZLLM_TRANSLATION_UNIVERSAL_ENABLED")); v != "" {
		on, perr := strconv.ParseBool(v)
		if perr != nil {
			return fmt.Errorf("EZLLM_TRANSLATION_UNIVERSAL_ENABLED=%q is not a boolean (want true/false/1/0): %w", v, perr)
		}
		cfg.Translation.UniversalEnabled = on
	}

	// ── data dir + store ──
	dbPath := filepath.Join(cfg.DataDir, "ezllm.sqlite")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	openCtx, cancelOpen := context.WithTimeout(ctx, 30*time.Second)
	defer cancelOpen()

	db, err := store.Open(openCtx, store.Options{
		Path:      dbPath,
		BatchSize: cfg.Ledger.BatchSize,
		BatchWait: cfg.LedgerBatchWait,
	})
	if err != nil {
		return err
	}
	defer db.Close()
	log.Info("store ready", "path", dbPath, "schema", db.SchemaVersion())

	// Seed accounts + client tokens from config (idempotent; config is bootstrap
	// only — everything after M3 is managed through the admin API).
	if err := seed(openCtx, db, cfg, log); err != nil {
		return fmt.Errorf("seed: %w", err)
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
	// gemini-web hops are rewritten to the in-process bridge; its base is the
	// server's own listen address (loopback always works in-process, even
	// when the listener binds all interfaces).
	bridgeHost := strings.TrimPrefix(addr, ":")
	if bridgeHost == "" {
		bridgeHost = "127.0.0.1"
	}
	if strings.HasPrefix(bridgeHost, ":") {
		bridgeHost = "127.0.0.1" + bridgeHost
	}
	registry.Register(provider.NewGeminiWeb("http://"+bridgeHost+"/internal/geminiweb/v1", client))
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
	ruleEngine, err := rules.NewEngine(
		storeAdapter{db}, storeAdapter{db}, rules.SystemClock, 30*time.Second, true)
	if err != nil {
		log.Warn("rules engine failed to load at boot; running unrestricted (ticker will retry)", "err", err)
		ruleEngine, err = rules.NewEngine(emptyRuleSource{}, storeAdapter{db}, rules.SystemClock, 30*time.Second, true)
		if err != nil {
			return fmt.Errorf("rules engine: %w", err) // permissive engine failing = real bug
		}
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
	resolver = resolver.WithUniversal(cfg.Translation.UniversalEnabled)
	log.Info("translation", "enabled", cfg.Translation.Enabled,
		"universal_enabled", cfg.Translation.UniversalEnabled,
		"responses_wire_models", cfg.Translation.ResponsesModels)

	go refreshProviderModels(ctx, db, registry, cfg.ModelRefresh.Interval, log)

	// EZLLM_USAGE_TZ sets the default zone for group_by=day bucketing so the
	// dashboard's day view agrees with a usage rule's day. Validated HERE,
	// at boot, with the real resolver — not with a regex. A typo must be a
	// startup error: the C5 lesson is that a tz which fails to resolve and
	// falls back silently is an 8-hour window shift with nothing pointing at
	// it, and the operator just sees a rule that "is not working".
	//
	// Per-request ?tz= still overrides it; this is only the default.
	usageTZ := strings.TrimSpace(os.Getenv("EZLLM_USAGE_TZ"))
	if usageTZ != "" {
		if _, err := time.LoadLocation(usageTZ); err != nil {
			return fmt.Errorf("EZLLM_USAGE_TZ=%q does not resolve to a time zone on this host "+
				"(use an IANA name like Asia/Kuala_Lumpur, or unset it for UTC): %w", usageTZ, err)
		}
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
	})

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE streams are unbounded by design (M7 tunes the rest).
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "surfaces", "openai,anthropic,responses")
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
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Error("http shutdown", "err", err)
	}
	// Stop the rules-engine ticker first so it cannot reload against a closing
	// store, then db.Close() drains the ledger queue and flushes the pools.
	ruleEngine.Close()
	if err := db.Close(); err != nil {
		return fmt.Errorf("store close: %w", err)
	}
	log.Info("stopped")
	return nil
}

func refreshProviderModels(ctx context.Context, db *store.DB, registry *provider.Registry, interval string, log *slog.Logger) {
	d, err := time.ParseDuration(interval)
	if err != nil || d < time.Minute {
		log.Warn("model refresh disabled: invalid interval", "interval", interval)
		return
	}
	refresh := func() {
		accounts, err := db.ListAccounts(ctx)
		if err != nil {
			log.Warn("model refresh: list accounts failed", "err", err)
			return
		}
		for _, summary := range accounts {
			if !summary.Enabled {
				continue
			}
			keys, err := db.ListKeys(ctx, summary.ID)
			if err != nil {
				log.Warn("model refresh: list keys failed", "account", summary.Namespace, "err", err)
				continue
			}
			var key string
			for _, k := range keys {
				if k.Enabled {
					_, key, err = db.KeyForTest(ctx, k.ID)
					if err == nil {
						break
					}
				}
			}
			if key == "" {
				continue
			}
			kind, err := provider.ParseKind(summary.Kind)
			if err != nil {
				log.Warn("model refresh: unknown provider kind", "account", summary.Namespace, "err", err)
				continue
			}
			adapter, err := registry.Get(kind)
			if err != nil {
				log.Warn("model refresh: adapter unavailable", "account", summary.Namespace, "err", err)
				continue
			}
			acct := provider.Account{
				ID: summary.ID, Name: summary.Name, Namespace: summary.Namespace,
				Kind: kind, BaseURL: summary.BaseURL, Enabled: summary.Enabled,
				RequiresSessionHeader: summary.RequiresSession,
				ProbeDelay:            time.Duration(summary.ProbeDelayMS) * time.Millisecond,
				QuotaMode:             summary.QuotaMode, CapWindow: summary.CapWindow,
				CapTokens: summary.CapTokens, Notes: summary.Notes,
			}
			models, err := adapter.ListModels(ctx, acct, key)
			if err != nil {
				log.Warn("model refresh failed; keeping last catalog", "account", summary.Namespace, "err", err)
				continue
			}
			if err := db.UpsertModels(ctx, summary.ID, models); err != nil {
				log.Warn("model refresh: persist failed", "account", summary.Namespace, "err", err)
				continue
			}
			log.Info("model catalog refreshed", "account", summary.Namespace, "models", len(models))
		}
	}
	refresh()
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
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
func seed(ctx context.Context, db *store.DB, cfg *config.Config, log *slog.Logger) error {
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
		if models, err := syncModels(ctx, db, cfg, acct); err != nil {
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
func syncModels(ctx context.Context, db *store.DB, cfg *config.Config, acct provider.Account) ([]provider.ModelInfo, error) {
	ad, err := provider.NewRegistry(upstreamClient(cfg)).Get(acct.Kind)
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

// upstreamClient builds the HTTP client for provider calls. No client-level
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

// Timeout by default: streaming responses are unbounded, and per-request
// deadlines come from the caller's context.
func upstreamClient(cfg *config.Config) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: cfg.UpstreamTimeout}
}
