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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/c4lyp5o/ezllm/internal/config"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
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
	registry := provider.NewRegistry(client)
	dispatcher := proxy.NewDispatcher(client, registry)
	dispatcher.SetRetries(cfg.Retry.Retries)
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

	go refreshProviderModels(ctx, db, registry, cfg.ModelRefresh.Interval, log)

	srv := server.New(server.Options{
		Auth: db, Resolver: resolver, Dispatcher: dispatcher, DB: db,
		Registry: registry, Log: log, Rules: ruleEngine,
		MaxBodyMiB:  cfg.MaxBodyMiB,
		CORSOrigins: nil,
	})

	addr := cfg.Listen
	if env := strings.TrimSpace(os.Getenv("EZLLM_ADDR")); env != "" {
		addr = env
	}
	if *addrOverride != "" {
		addr = *addrOverride
	}

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
