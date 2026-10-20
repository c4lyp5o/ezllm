package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/server"
	"github.com/c4lyp5o/ezllm/internal/store"
)

const (
	envDashboardPassword     = "EZLLM_DASHBOARD_PASSWORD"
	envDashboardPasswordFile = "EZLLM_DASHBOARD_PASSWORD_FILE"
)

// dashboardPasswordFromEnv reads the operator-supplied first password:
// EZLLM_DASHBOARD_PASSWORD wins, else the file EZLLM_DASHBOARD_PASSWORD_FILE
// names (Docker/compose secrets; one trailing newline is trimmed). Empty
// means neither is set.
func dashboardPasswordFromEnv() (string, error) {
	if v := os.Getenv(envDashboardPassword); v != "" {
		return v, nil
	}
	path := strings.TrimSpace(os.Getenv(envDashboardPasswordFile))
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", envDashboardPasswordFile, err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// prepareDashboardSetup runs once per boot. With a password already set it
// does nothing (the env value never overwrites one — change it in Settings).
// On first boot an env password completes setup directly; otherwise it
// mints the setup code main prints in the first-boot banner.
func prepareDashboardSetup(ctx context.Context, db *store.DB, log *slog.Logger) (string, error) {
	pw, err := dashboardPasswordFromEnv()
	if err != nil {
		return "", err
	}
	first, err := db.DashboardFirstBoot(ctx)
	if err != nil {
		return "", fmt.Errorf("read dashboard setup state: %w", err)
	}
	if !first {
		if pw != "" {
			log.Debug("dashboard password from env ignored: a password is already set")
		}
		return "", nil
	}
	if pw != "" {
		if err := db.CompleteDashboardSetup(ctx, pw); err != nil {
			return "", fmt.Errorf("%s: %w", envDashboardPassword, err)
		}
		log.Info("dashboard password set from environment")
		return "", nil
	}
	return server.NewSetupCode()
}
