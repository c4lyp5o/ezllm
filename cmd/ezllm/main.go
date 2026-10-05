// Command ezllm is Calypso's lightweight LLM router.
//
// M1: config load (fail-fast validation), OpenAI-compatible surface
// (/healthz, /v1/models, /v1/chat/completions stub), graceful shutdown.
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
	"syscall"
	"time"

	"github.com/c4lyp5o/ezllm/internal/config"
	"github.com/c4lyp5o/ezllm/internal/server"
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

	addr := cfg.Listen
	if v := os.Getenv("EZLLM_ADDR"); v != "" {
		addr = v
	}
	if *addrOverride != "" {
		addr = *addrOverride
	}

	srv := server.New(cfg, log)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE streams are unbounded by design (M6 tunes the rest).
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening",
			"addr", addr,
			"aliases", len(cfg.Aliases),
			"providers", len(cfg.Providers),
			"clients", len(cfg.ClientTokens),
		)
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
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("stopped")
	return nil
}
