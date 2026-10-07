package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const DefaultDashboardPassword = "STRONGPASSWORD"

var ErrInvalidDashboardPassword = errors.New("store: invalid dashboard password")

func (d *DB) EnsureDashboardPassword(ctx context.Context) error {
	var hash string
	err := d.r.QueryRowContext(ctx, `SELECT password_hash FROM dashboard_auth WHERE id=1`).Scan(&hash)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	encoded, err := bcrypt.GenerateFromPassword([]byte(DefaultDashboardPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO dashboard_auth (id, password_hash) VALUES (1, ?)
ON CONFLICT(id) DO NOTHING`, string(encoded))
		return err
	})
}

func (d *DB) DashboardLogin(ctx context.Context, password string) (string, error) {
	var hash string
	if err := d.r.QueryRowContext(ctx, `SELECT password_hash FROM dashboard_auth WHERE id=1`).Scan(&hash); err != nil {
		return "", err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", ErrInvalidDashboardPassword
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	expires := time.Now().UTC().Add(12 * time.Hour)
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO dashboard_sessions (token_hash, expires_at) VALUES (?, ?)`, hashDashboardToken(token), expires.Format(time.RFC3339Nano))
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func (d *DB) DashboardSession(ctx context.Context, token string) bool {
	if strings.TrimSpace(token) == "" {
		return false
	}
	var expires string
	err := d.r.QueryRowContext(ctx, `SELECT expires_at FROM dashboard_sessions WHERE token_hash=?`, hashDashboardToken(token)).Scan(&expires)
	if err != nil {
		return false
	}
	when, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !when.After(time.Now().UTC()) {
		_, _ = d.w.ExecContext(ctx, `DELETE FROM dashboard_sessions WHERE token_hash=?`, hashDashboardToken(token))
		return false
	}
	return true
}

func (d *DB) ChangeDashboardPassword(ctx context.Context, currentPassword, newPassword string) error {
	var currentHash string
	if err := d.r.QueryRowContext(ctx, `SELECT password_hash FROM dashboard_auth WHERE id=1`).Scan(&currentHash); err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(currentHash), []byte(currentPassword)) != nil {
		return ErrInvalidDashboardPassword
	}
	if len(newPassword) < 12 {
		return errors.New("dashboard password must be at least 12 characters")
	}
	encoded, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE dashboard_auth SET password_hash=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=1`, string(encoded)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM dashboard_sessions`)
		return err
	})
}

func hashDashboardToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
