package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/backup"
	"github.com/c4lyp5o/ezllm/internal/config"
	"github.com/c4lyp5o/ezllm/internal/settings"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func operatorSettings(ctx context.Context, db *store.DB, c *config.Config) (*settings.Manager, error) {
	v := settings.Defaults()
	sources := map[string]string{}
	// Load's validated values preserve legacy YAML normalization. Explicitness
	// determines provenance, independently of the defaults seeded by Load.
	v.Translation = c.Translation.Enabled
	v.CatalogInterval = c.ModelRefresh.Interval
	v.Retries = c.Retry.Retries
	v.AttemptBudget = c.Retry.RequestAttemptBudget
	v.DispatchDeadline = c.Retry.DispatchDeadlineStr
	v.Cooldown429 = c.Cooldown.On429
	v.Cooldown5xx = c.Cooldown.On5xx
	v.CooldownStrikes = c.Cooldown.MaxPerKey
	if c.Explicit["usage_timezone"] || c.UsageTZ != "" {
		v.UsageTZ = c.UsageTZ
	}
	if c.PriceRefresh != nil {
		v.PriceRefresh = *c.PriceRefresh
	}
	if c.AutoProbe != nil {
		v.AutoProbe = *c.AutoProbe
	}
	for k, path := range map[string]string{"usage_timezone": "usage_timezone", "translation_enabled": "translation.enabled", "catalog_interval": "model_refresh.interval", "retries": "retry.retries", "attempt_budget": "retry.request_attempt_budget", "dispatch_deadline": "retry.dispatch_deadline", "cooldown_429": "cooldown.on_429", "cooldown_5xx": "cooldown.on_5xx", "cooldown_strikes": "cooldown.max_per_key", "automatic_probing": "automatic_probing", "price_refresh": "price_refresh"} {
		if c.Explicit[path] {
			sources[k] = "yaml"
		}
	}
	if c.Explicit["translation.universal_enabled"] {
		sources["translation_enabled"] = "yaml"
	}
	if raw := strings.TrimSpace(os.Getenv("EZLLM_USAGE_TZ")); raw != "" {
		v.UsageTZ = raw
		sources["usage_timezone"] = "environment"
	}
	if os.Getenv("EZLLM_TRANSLATION_ENABLED") != "" || os.Getenv("EZLLM_TRANSLATION_UNIVERSAL_ENABLED") != "" {
		sources["translation_enabled"] = "environment"
	}
	if raw := os.Getenv("EZLLM_PRICE_REFRESH"); raw != "" {
		v.PriceRefresh = raw != "off"
		sources["price_refresh"] = "environment"
	}
	if raw := os.Getenv("EZLLM_COMBO_AUTOPROBE"); raw != "" {
		v.AutoProbe = raw != "off"
		sources["automatic_probing"] = "environment"
	}
	return settings.New(ctx, db, v, sources)
}

func configuredBackups(db *store.DB, c *config.Config, master string) (*backup.Manager, error) {
	dir := c.Backup.Directory
	if v := os.Getenv("EZLLM_BACKUP_DIR"); v != "" {
		dir = v
	}
	secretFile := c.Backup.SecretFile
	if v := os.Getenv("EZLLM_BACKUP_SECRET_FILE"); v != "" {
		secretFile = v
	}
	secret := ""
	if secretFile != "" {
		raw, err := os.ReadFile(secretFile)
		if err != nil {
			return nil, fmt.Errorf("read backup secret file: %w", err)
		}
		secret = strings.TrimSpace(string(raw))
	}
	return backup.New(db, dir, secret, master), nil
}

// A missing backup volume must not take routing down or prevent unrelated
// policy edits. Validate storage on the transition that enables automation;
// each actual job checks it again and reports failures independently.
func backupSettingsCheck(operator *settings.Manager, backups *backup.Manager) func(settings.Values) error {
	return func(v settings.Values) error {
		if v.BackupEnabled && !operator.Current().BackupEnabled {
			return backups.Check()
		}
		return nil
	}
}
