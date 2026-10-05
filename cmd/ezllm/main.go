// Command ezllm is Calypso's lightweight LLM router.
//
// M2: config + SQLite store (WAL, single writer) + encrypted provider keys +
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

	level := slog.LevelInfo
	switch os.Getenv("EZLLM_LOG_LEVEL") {
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

	// ── master key: lives OUTSIDE data/ so copying the DB alone leaks nothing ──
	master, err := loadMasterKey(cfg)
	if err != nil {
		return err
	}

	// ── data dir + store ──
	dataDir := cfg.DataDir
	if v := os.Getenv("EZLLM_DATA_DIR"); v != "" {
		dataDir = v
	}
	dbPath := filepath.Join(dataDir, "ezllm.sqlite")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	openCtx, cancelOpen := context.WithTimeout(ctx, 30*time.Second)
	defer cancelOpen()

	db, err := store.Open(openCtx, store.Options{
		Path:      dbPath,
		MasterKey: master,
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
	resolver := router.New(db)

	srv := server.New(server.Options{
		Auth: db, Resolver: resolver, Dispatcher: dispatcher, DB: db, Log: log,
		MaxBodyMiB: cfg.MaxBodyMiB,
	})

	addr := cfg.Listen
	if v := os.Getenv("EZLLM_ADDR"); v != "" {
		addr = v
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
	// db.Close() drains the ledger queue and flushes before closing the pools.
	if err := db.Close(); err != nil {
		return fmt.Errorf("store close: %w", err)
	}
	log.Info("stopped")
	return nil
}

// seed registers accounts, their keys, and client tokens from config on first
// boot. Idempotent: re-running updates in place and never duplicates.
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
		for _, k := range p.Keys {
			plain := strings.TrimSpace(os.Getenv(k.Env))
			if plain == "" {
				log.Warn("provider key env not set — skipping", "provider", name, "env", k.Env)
				continue
			}
			label := k.Label
			if label == "" {
				label = k.Env
			}
			// Only add when this key isn't already stored for the account
			// (compare by hint, so a restart never duplicates rows).
			if exists, err := db.HasKeyByHint(ctx, id, store.KeyHint(plain)); err == nil && exists {
				continue
			}
			if _, _, err := db.AddKey(ctx, id, label, plain); err != nil {
				return fmt.Errorf("key for %q: %w", name, err)
			}
			log.Info("seeded provider key", "provider", name, "label", label, "hint", store.KeyHint(plain))
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

	for _, t := range cfg.ClientTokens {
		plain := strings.TrimSpace(os.Getenv(t.TokenEnv))
		if plain == "" {
			return fmt.Errorf("client_tokens[%s]: env %s is not set", t.Name, t.TokenEnv)
		}
		roles := t.Roles
		if roles == "" {
			roles = "infer"
		}
		if err := db.UpsertClientToken(ctx, t.Name, plain, roles); err != nil {
			return fmt.Errorf("client token %q: %w", t.Name, err)
		}
	}
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

// loadMasterKey resolves the field-encryption master key.
// Precedence: EZLLM_MASTER_KEY env > master_key_file > ~/.hermes/secrets/ezllm-master.key
// The file may carry a trailing newline; it is trimmed (a footgun hit while
// reverse-engineering omniroute's equivalent scheme).
func loadMasterKey(cfg *config.Config) (string, error) {
	if v := strings.TrimSpace(os.Getenv("EZLLM_MASTER_KEY")); v != "" {
		return v, nil
	}
	path := cfg.MasterKeyFile
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("no master key: set EZLLM_MASTER_KEY or master_key_file")
		}
		path = filepath.Join(home, ".hermes", "secrets", "ezllm-master.key")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("master key %s: %w\n  create it with: umask 077 && openssl rand -hex 32 > %s", path, err, path)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("master key file %s is empty", path)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("master key %s must be 0600 (got %04o) — it protects every provider key", path, fi.Mode().Perm())
	}
	return key, nil
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
