package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// NotificationChannel is one external push channel for bell events (v32).
// Its secret fields are held only in secrets, which no JSON carries: the
// API names them in SecretsSet, and the push worker reads them with Secrets.
type NotificationChannel struct {
	ID          int64             `json:"id"`
	Kind        string            `json:"kind"`
	Name        string            `json:"name"`
	Enabled     bool              `json:"enabled"`
	MinSeverity string            `json:"min_severity"`
	Config      map[string]string `json:"config"`
	SecretsSet  []string          `json:"secrets_set"`
	LastStatus  string            `json:"last_status"`
	LastError   string            `json:"last_error,omitempty"`
	LastSentAt  *string           `json:"last_sent_at"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
	secrets     map[string]string
}

// Secrets returns a copy of the channel's decrypted secret fields.
func (c NotificationChannel) Secrets() map[string]string {
	out := make(map[string]string, len(c.secrets))
	for k, v := range c.secrets {
		out[k] = v
	}
	return out
}

// WithSecrets returns c holding secrets — for a draft that is tested before
// it is saved.
func (c NotificationChannel) WithSecrets(secrets map[string]string) NotificationChannel {
	c.secrets = secrets
	c.SecretsSet = secretNames(secrets)
	return c
}

// MaxNotificationChannels bounds the table: a handful of phones and rooms.
const MaxNotificationChannels = 10

func secretNames(m map[string]string) []string {
	names := []string{}
	for k, v := range m {
		if v != "" {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return names
}

func (d *DB) sealSecrets(secrets map[string]string) (string, error) {
	clean := map[string]string{}
	for k, v := range secrets {
		if v = strings.TrimSpace(v); v != "" {
			clean[k] = v
		}
	}
	if len(clean) == 0 {
		return "", nil
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return "", err
	}
	if d.crypto == nil {
		return string(b), nil
	}
	return d.crypto.Encrypt(string(b))
}

func (d *DB) openSecrets(stored string) (map[string]string, error) {
	out := map[string]string{}
	if stored == "" {
		return out, nil
	}
	plain := stored
	if strings.HasPrefix(stored, cryptoPrefix) {
		if d.crypto == nil {
			return nil, errors.New("store: notification channel secrets are encrypted and no master key is loaded")
		}
		var err error
		if plain, err = d.crypto.Decrypt(stored); err != nil {
			return nil, fmt.Errorf("store: notification channel secrets: %w", err)
		}
	}
	if err := json.Unmarshal([]byte(plain), &out); err != nil {
		return nil, fmt.Errorf("store: notification channel secrets: %w", err)
	}
	return out, nil
}

const channelColumns = `id, kind, name, enabled, min_severity, config, secrets, last_status, last_error, last_sent_at, created_at, updated_at`

func (d *DB) scanChannel(row interface{ Scan(...any) error }) (NotificationChannel, error) {
	var c NotificationChannel
	var config, secrets string
	if err := row.Scan(&c.ID, &c.Kind, &c.Name, &c.Enabled, &c.MinSeverity, &config, &secrets,
		&c.LastStatus, &c.LastError, &c.LastSentAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return c, err
	}
	c.Config = map[string]string{}
	if err := json.Unmarshal([]byte(config), &c.Config); err != nil {
		return c, fmt.Errorf("store: notification channel %d config: %w", c.ID, err)
	}
	var err error
	if c.secrets, err = d.openSecrets(secrets); err != nil {
		return c, err
	}
	c.SecretsSet = secretNames(c.secrets)
	return c, nil
}

// ListNotificationChannels returns every channel, oldest first.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]NotificationChannel, error) {
	rows, err := d.r.QueryContext(ctx, `SELECT `+channelColumns+` FROM notification_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotificationChannel{}
	for rows.Next() {
		c, err := d.scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannel returns one channel, or ErrNotFound.
func (d *DB) NotificationChannel(ctx context.Context, id int64) (NotificationChannel, error) {
	c, err := d.scanChannel(d.r.QueryRowContext(ctx, `SELECT `+channelColumns+` FROM notification_channels WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// CreateNotificationChannel stores a validated channel and returns its id.
func (d *DB) CreateNotificationChannel(ctx context.Context, c NotificationChannel, secrets map[string]string) (int64, error) {
	config, err := json.Marshal(nonNil(c.Config))
	if err != nil {
		return 0, err
	}
	sealed, err := d.sealSecrets(secrets)
	if err != nil {
		return 0, err
	}
	var id int64
	err = d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_channels`).Scan(&n); err != nil {
			return err
		}
		if n >= MaxNotificationChannels {
			return fmt.Errorf("store: at most %d notification channels", MaxNotificationChannels)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO notification_channels (kind, name, enabled, min_severity, config, secrets) VALUES (?,?,?,?,?,?)`,
			c.Kind, c.Name, c.Enabled, c.MinSeverity, string(config), sealed)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// UpdateNotificationChannel replaces a channel's name, switch, severity,
// config and secrets (the caller merges kept secrets in). Its delivery
// status resets: it describes the old destination.
func (d *DB) UpdateNotificationChannel(ctx context.Context, c NotificationChannel, secrets map[string]string) error {
	config, err := json.Marshal(nonNil(c.Config))
	if err != nil {
		return err
	}
	sealed, err := d.sealSecrets(secrets)
	if err != nil {
		return err
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE notification_channels SET name=?, enabled=?, min_severity=?, config=?, secrets=?,
       last_status='', last_error='', last_sent_at=NULL, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
WHERE id=?`, c.Name, c.Enabled, c.MinSeverity, string(config), sealed, c.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteNotificationChannel removes a channel, or reports ErrNotFound.
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RecordChannelDelivery stores the outcome of the latest delivery: ok, or
// failed with a reason the notify package already cleared of secrets.
func (d *DB) RecordChannelDelivery(ctx context.Context, id int64, failure string) error {
	status := "ok"
	if failure != "" {
		status = "failed"
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
UPDATE notification_channels SET last_status=?, last_error=?, last_sent_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`,
			status, failure, id)
		return err
	})
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
