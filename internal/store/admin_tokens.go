package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ── client tokens ───────────────────────────────────────────────────────────

// TokenSummary is the admin-API token shape (hash never leaves the store).
type TokenSummary struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Hint          string   `json:"hint"`
	Roles         []string `json:"roles"`
	Enabled       bool     `json:"enabled"`
	CapWindow     string   `json:"cap_window"`
	CapTokens     int64    `json:"cap_tokens"`
	CapUSD        float64  `json:"cap_usd"` // priced spend per cap_window; 0 = unlimited
	AllowedModels string   `json:"allowed_models"`
	ExpiresAt     string   `json:"expires_at"`
	LastUsedAt    *string  `json:"last_used_at"`
	CreatedAt     string   `json:"created_at"`
	// CompressionProfileID: the key's own compression profile, used when the
	// request names none (header) — even on a direct namespace/model route.
	CompressionProfileID *int64 `json:"compression_profile_id"`
}

// ListTokens returns all client tokens (hints only).
func (d *DB) ListTokens(ctx context.Context) ([]TokenSummary, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT id, name, token_hash, roles, enabled, cap_window, cap_tokens, cap_usd,
       allowed_models, expires_at,
       -- no last_used_at column: derive from the ledger (calls.client is the
       -- token name, set by ClientByToken). No hot-path write needed.
       COALESCE((SELECT MAX(ts) FROM calls c WHERE c.client = client_tokens.name),
                (SELECT MAX(day) || 'T12:00:00.000Z' FROM calls_daily d WHERE d.client = client_tokens.name)),
       created_at, compression_profile_id
FROM client_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenSummary{}
	for rows.Next() {
		var t TokenSummary
		var hash, roles string
		var lastUsed sql.NullString
		var profile sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &hash, &roles, &t.Enabled,
			&t.CapWindow, &t.CapTokens, &t.CapUSD, &t.AllowedModels, &t.ExpiresAt, &lastUsed, &t.CreatedAt, &profile); err != nil {
			return nil, err
		}
		if profile.Valid && profile.Int64 > 0 {
			t.CompressionProfileID = &profile.Int64
		}
		t.Roles = splitCSV(roles)
		t.Hint = tokenHint(hash)
		if lastUsed.Valid && lastUsed.String != "" {
			t.LastUsedAt = &lastUsed.String
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TokenInput is the create payload; Token is plaintext and returned ONCE.
type TokenInput struct {
	Name          string   `json:"name"`
	Roles         []string `json:"roles"`
	Token         string   `json:"token"`
	AllowedModels string   `json:"allowed_models"`
	ExpiresAt     string   `json:"expires_at"`
}

// TokenRoles are the client-token roles, each a separate permission (none
// implies another): infer calls models on /v1, metrics reads GET /metrics,
// admin manages the gateway through /admin.
var TokenRoles = []string{"infer", "metrics", "admin"}

// NormalizeRoles validates roles and returns them deduplicated in TokenRoles
// order. A token needs at least one role.
func NormalizeRoles(roles []string) ([]string, error) {
	seen := map[string]bool{}
	for _, r := range roles {
		r = strings.TrimSpace(r)
		if !slices.Contains(TokenRoles, r) {
			return nil, fmt.Errorf("role %q must be one of %s", r, strings.Join(TokenRoles, ", "))
		}
		seen[r] = true
	}
	out := make([]string, 0, len(seen))
	for _, r := range TokenRoles {
		if seen[r] {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("a key needs at least one role")
	}
	return out, nil
}

// ValidateToken checks roles before writing.
func (in TokenInput) Validate() error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("name is required")
	}
	if len(in.Roles) > 0 {
		if _, err := NormalizeRoles(in.Roles); err != nil {
			return err
		}
	}
	if _, err := normalizeExpiry(in.ExpiresAt, time.UTC); err != nil { // validation: the zone cannot make it fail
		return err
	}
	return nil
}

// normalizeExpiry stores RFC3339 UTC. A bare YYYY-MM-DD means end of that day
// in the gateway's local timezone (the same day the budget windows use) so a
// date picker does not expire a key at midnight at the start.
func normalizeExpiry(s string, loc *time.Location) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t.Add(24*time.Hour - time.Second).UTC().Format(time.RFC3339), nil
	}
	return "", fmt.Errorf("expires_at must be RFC3339 or YYYY-MM-DD")
}

// Tokens are created, never overwritten: returning fresh plaintext for a name
// that already exists is unsafe and misleading.
func (d *DB) UpsertToken(ctx context.Context, in TokenInput) (id int64, plaintext string, err error) {
	if err := in.Validate(); err != nil {
		return 0, "", err
	}
	plaintext = in.Token
	if plaintext == "" {
		plaintext, err = GenerateToken()
		if err != nil {
			return 0, "", err
		}
	}
	roles := []string{"infer"}
	if len(in.Roles) > 0 {
		if roles, err = NormalizeRoles(in.Roles); err != nil {
			return 0, "", err
		}
	}
	expires, err := normalizeExpiry(in.ExpiresAt, d.usageLocation())
	if err != nil {
		return 0, "", err
	}
	allowed := strings.TrimSpace(in.AllowedModels)
	err = d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT name FROM client_tokens WHERE name=?`, in.Name).Scan(&existing)
		if err == nil {
			return &ErrConflict{What: fmt.Sprintf("token name %q already exists", in.Name)}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO client_tokens (name, token_hash, roles, enabled, allowed_models, expires_at)
VALUES (?,?,?,1,?,?)`,
			in.Name, HashToken(plaintext), strings.Join(roles, ","), allowed, expires)
		if err != nil && isUniqueViolation(err) {
			// The pre-check above is check-then-act, so a genuine race — two
			// concurrent POSTs with the same name — reaches this line. A raw
			// SQLite error here answered 500; map it to the SAME *ErrConflict
			// the pre-check returns, exactly as CreateAccount/writeAccount do
			// for their UNIQUE clashes. Note the known adjacent wart this
			// interacts with: a duplicate NAME can also answer 201 with a
			// plaintext that authenticates nothing (no row created) — both
			// live in the check-then-act gap and any deeper fix addresses them
			// together.
			return &ErrConflict{What: fmt.Sprintf("token name %q already exists", in.Name)}
		}
		return err
	})
	if err != nil {
		return 0, "", err
	}
	err = d.r.QueryRowContext(ctx, `SELECT id FROM client_tokens WHERE name=?`, in.Name).Scan(&id)
	return id, plaintext, err
}

// UpdateToken patches enabled/roles/caps/policy.
func (d *DB) UpdateToken(ctx context.Context, id int64, enabled *bool, roles []string, capTokens *int64, capWindow string, allowedModels *string, expiresAt *string) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if enabled != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE client_tokens SET enabled=? WHERE id=?`, boolToInt(*enabled), id); err != nil {
				return err
			}
		}
		if roles != nil {
			norm, err := NormalizeRoles(roles)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE client_tokens SET roles=? WHERE id=?`, strings.Join(norm, ","), id); err != nil {
				return err
			}
		}
		if capTokens != nil {
			if *capTokens < 0 {
				return errors.New("cap_tokens must be >= 0")
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE client_tokens SET cap_tokens=? WHERE id=?`, *capTokens, id); err != nil {
				return err
			}
		}
		if capWindow != "" {
			switch capWindow {
			case "5h", "daily", "weekly", "monthly":
			default:
				return errors.New("cap_window must be 5h|daily|weekly|monthly")
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE client_tokens SET cap_window=? WHERE id=?`, capWindow, id); err != nil {
				return err
			}
		}
		if allowedModels != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE client_tokens SET allowed_models=? WHERE id=?`, strings.TrimSpace(*allowedModels), id); err != nil {
				return err
			}
		}
		if expiresAt != nil {
			exp, err := normalizeExpiry(*expiresAt, d.usageLocation())
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE client_tokens SET expires_at=? WHERE id=?`, exp, id); err != nil {
				return err
			}
		}
		// Verify the row existed: a PATCH on a missing id must 404, not no-op.
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM client_tokens WHERE id=?`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetTokenCompressionProfile sets (or with nil clears) a key's compression
// profile. An id that names no profile is ErrNotFound.
func (d *DB) SetTokenCompressionProfile(ctx context.Context, id int64, profileID *int64) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var v any
		if profileID != nil && *profileID > 0 {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM compression_profiles WHERE id=?`, *profileID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("%w: compression profile %d", ErrNotFound, *profileID)
			}
			v = *profileID
		}
		res, err := tx.ExecContext(ctx, `UPDATE client_tokens SET compression_profile_id=? WHERE id=?`, v, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// TokenCompressionProfileID returns the compression profile a key carries, or
// 0 when it has none (or the key is gone).
func (d *DB) TokenCompressionProfileID(ctx context.Context, name string) int64 {
	var id sql.NullInt64
	_ = d.r.QueryRowContext(ctx, `SELECT compression_profile_id FROM client_tokens WHERE name=?`, name).Scan(&id)
	if id.Valid {
		return id.Int64
	}
	return 0
}

// DeleteToken revokes a client token permanently.
func (d *DB) DeleteToken(ctx context.Context, id int64) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM client_tokens WHERE id=?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ── compression profiles ─────────────────────────────────────────────────────

// CompressionProfile is a full pipeline row: stages JSON plus the safety
// gates (docs/design/compression-design.md). CRUD lives in compression.go.
type CompressionProfile struct {
	ID                int64           `json:"id"`
	Name              string          `json:"name"`
	Enabled           bool            `json:"enabled"`
	Stages            json.RawMessage `json:"stages"`
	ExemptLastTurn    bool            `json:"exempt_last_turn"`
	MinCompressRatio  float64         `json:"min_compress_ratio"`
	FailOpen          bool            `json:"fail_open"`
	AutoTriggerTokens int64           `json:"auto_trigger_tokens"`
	// FreshToolOutput: "keep" (default) leaves a trailing tool result for
	// the exempt last turn; "compress" lets tool-output engines trim it on
	// its first send (it is then frozen by the memo like any message).
	FreshToolOutput string `json:"fresh_tool_output"`
	Notes           string `json:"notes,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
}

// ListCompressionProfiles returns every profile with its full pipeline —
// the combo editor and the Compression page read the same row.
func (d *DB) ListCompressionProfiles(ctx context.Context) ([]CompressionProfile, error) {
	rows, err := d.r.QueryContext(ctx, profileSelect+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CompressionProfile{}
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}
