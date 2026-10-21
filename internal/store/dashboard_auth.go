package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// legacyDefaultPassword is the factory password seeded before schema v26. It
// is never seeded now; a database whose hash still matches it is sent back
// through first-boot setup, because the value is public.
const legacyDefaultPassword = "STRONGPASSWORD"

// MinDashboardPasswordLen is the shortest dashboard password setup or a
// change accepts.
const MinDashboardPasswordLen = 12

var (
	ErrInvalidDashboardPassword = errors.New("store: invalid dashboard password")
	// ErrDashboardSetupRequired: no password exists yet (first boot), so
	// nothing can log in until setup completes.
	ErrDashboardSetupRequired = errors.New("store: dashboard setup required")
	// ErrDashboardSetupDone: setup already ran; it never runs twice.
	ErrDashboardSetupDone = errors.New("store: dashboard password already set")
	// ErrDashboardPasswordTooShort carries the "at least 12 characters"
	// wording the HTTP layer matches on.
	ErrDashboardPasswordTooShort = fmt.Errorf("store: dashboard password must be at least %d characters", MinDashboardPasswordLen)
)

// EnsureDashboardPassword creates the dashboard_auth row on first boot with
// no password and first_boot=1, so the dashboard shows only its set-password
// page until setup completes. A row still holding the legacy factory
// password is reset to that same first-boot state (and its sessions
// revoked): the password is published, so it cannot stay a credential.
func (d *DB) EnsureDashboardPassword(ctx context.Context) error {
	var hash string
	var firstBoot int
	err := d.r.QueryRowContext(ctx, `SELECT password_hash, first_boot FROM dashboard_auth WHERE id=1`).Scan(&hash, &firstBoot)
	if errors.Is(err, sql.ErrNoRows) {
		return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
INSERT INTO dashboard_auth (id, password_hash, first_boot) VALUES (1, '', 1)
ON CONFLICT(id) DO NOTHING`)
			return err
		})
	}
	if err != nil {
		return err
	}
	if firstBoot == 1 || hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(legacyDefaultPassword)) != nil {
		return nil
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE dashboard_auth SET password_hash='', first_boot=1, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=1`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM dashboard_sessions`)
		return err
	})
}

// DashboardFirstBoot reports whether the dashboard password has never been
// set. One indexed read, no bcrypt — the login page asks it on every load.
func (d *DB) DashboardFirstBoot(ctx context.Context) (bool, error) {
	var firstBoot int
	if err := d.r.QueryRowContext(ctx, `SELECT first_boot FROM dashboard_auth WHERE id=1`).Scan(&firstBoot); err != nil {
		return false, err
	}
	return firstBoot == 1, nil
}

// CompleteDashboardSetup sets the first dashboard password and flips
// first_boot in the same statement. The WHERE first_boot=1 guard makes it
// one-shot: a second call (or a race between two) gets ErrDashboardSetupDone.
func (d *DB) CompleteDashboardSetup(ctx context.Context, password string) error {
	if len(password) < MinDashboardPasswordLen {
		return ErrDashboardPasswordTooShort
	}
	encoded, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
UPDATE dashboard_auth SET password_hash=?, first_boot=0, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
WHERE id=1 AND first_boot=1`, string(encoded))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrDashboardSetupDone
		}
		return nil
	})
}

func (d *DB) DashboardLogin(ctx context.Context, password string) (string, error) {
	var hash string
	var firstBoot int
	if err := d.r.QueryRowContext(ctx, `SELECT password_hash, first_boot FROM dashboard_auth WHERE id=1`).Scan(&hash, &firstBoot); err != nil {
		return "", err
	}
	if firstBoot == 1 {
		return "", ErrDashboardSetupRequired
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
		// Expired sessions were only deleted when someone presented that
		// exact token, so the table grew by one row per login. Prune here.
		if _, err := tx.ExecContext(ctx, `DELETE FROM dashboard_sessions WHERE expires_at < ?`,
			time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO dashboard_sessions (token_hash, expires_at) VALUES (?, ?)`, HashToken(token), expires.Format(time.RFC3339Nano))
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
	err := d.r.QueryRowContext(ctx, `SELECT expires_at FROM dashboard_sessions WHERE token_hash=?`, HashToken(token)).Scan(&expires)
	if err != nil {
		return false
	}
	when, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !when.After(time.Now().UTC()) {
		_, _ = d.w.ExecContext(ctx, `DELETE FROM dashboard_sessions WHERE token_hash=?`, HashToken(token))
		return false
	}
	return true
}

// EndDashboardSession revokes one dashboard session (sign-out). Unknown or
// empty tokens are a no-op: signing out twice is not an error.
func (d *DB) EndDashboardSession(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	_, err := d.w.ExecContext(ctx, `DELETE FROM dashboard_sessions WHERE token_hash=?`, HashToken(token))
	return err
}

func (d *DB) ChangeDashboardPassword(ctx context.Context, currentPassword, newPassword string) error {
	var currentHash string
	if err := d.r.QueryRowContext(ctx, `SELECT password_hash FROM dashboard_auth WHERE id=1`).Scan(&currentHash); err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(currentHash), []byte(currentPassword)) != nil {
		return ErrInvalidDashboardPassword
	}
	if len(newPassword) < MinDashboardPasswordLen {
		return ErrDashboardPasswordTooShort
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
