package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// ErrNotFound is returned for missing rows (accounts, keys, models).
var ErrNotFound = errors.New("store: not found")

// KeyPick is a decrypted key plus the identity the ledger records.
// Plaintext must never be logged.
type KeyPick struct {
	ID        int64
	Hint      string
	Plaintext string
}

// ── accounts ────────────────────────────────────────────────────────────────

// AccountByNamespace returns the account registered under a namespace.
func (d *DB) AccountByNamespace(ctx context.Context, ns string) (*provider.Account, error) {
	row := d.r.QueryRowContext(ctx, `
SELECT id, name, namespace, kind, base_url, enabled, requires_session_header,
       COALESCE(custom_headers,''), COALESCE(probe_delay_ms,400)
FROM accounts WHERE namespace = ?`, ns)
	var (
		a             provider.Account
		kind          string
		enabled       int
		needsSession  int
		customHeaders string
		probeDelay    int
	)
	if err := row.Scan(&a.ID, &a.Name, &a.Namespace, &kind, &a.BaseURL, &enabled,
		&needsSession, &customHeaders, &probeDelay); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	k, err := provider.ParseKind(kind)
	if err != nil {
		return nil, err
	}
	a.Kind = k
	a.Enabled = enabled == 1
	a.RequiresSessionHeader = needsSession == 1
	a.ProbeDelay = time.Duration(probeDelay) * time.Millisecond
	if customHeaders != "" {
		var h map[string]string
		if err := json.Unmarshal([]byte(customHeaders), &h); err == nil {
			a.CustomHeaders = h
		}
	}
	return &a, nil
}

// AccountByID returns an account by primary key.
func (d *DB) AccountByID(ctx context.Context, id int64) (*provider.Account, error) {
	row := d.r.QueryRowContext(ctx, `
SELECT id, name, namespace, kind, base_url, enabled, requires_session_header,
       COALESCE(custom_headers,''), COALESCE(probe_delay_ms,400)
FROM accounts WHERE id = ?`, id)
	var (
		a             provider.Account
		kind          string
		enabled       int
		needsSession  int
		customHeaders string
		probeDelay    int
	)
	if err := row.Scan(&a.ID, &a.Name, &a.Namespace, &kind, &a.BaseURL, &enabled,
		&needsSession, &customHeaders, &probeDelay); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	k, err := provider.ParseKind(kind)
	if err != nil {
		return nil, err
	}
	a.Kind = k
	a.Enabled = enabled == 1
	a.RequiresSessionHeader = needsSession == 1
	a.ProbeDelay = time.Duration(probeDelay) * time.Millisecond
	if customHeaders != "" {
		var h map[string]string
		if err := json.Unmarshal([]byte(customHeaders), &h); err == nil {
			a.CustomHeaders = h
		}
	}
	return &a, nil
}

// ── keys ────────────────────────────────────────────────────────────────────

// PickKey returns the first enabled, non-cooled-down key for an account,
// decrypting it. M2 uses simple ordering; M4 replaces this with rotation +
// cooldown + quota exclusion (the interface stays identical).
func (d *DB) PickKey(ctx context.Context, accountID int64) (KeyPick, error) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	rows, err := d.r.QueryContext(ctx, `
SELECT k.id, k.key_hint, k.key_ct
FROM provider_keys k
LEFT JOIN key_cooldowns c ON c.provider_key_id = k.id
WHERE k.account_id = ? AND k.enabled = 1
  AND (c.until_ts IS NULL OR c.until_ts <= ?)
ORDER BY k.id
LIMIT 8`, accountID, now)
	if err != nil {
		return KeyPick{}, err
	}
	defer rows.Close()

	var lastErr error
	for rows.Next() {
		var p KeyPick
		var ct string
		if err := rows.Scan(&p.ID, &p.Hint, &ct); err != nil {
			return KeyPick{}, err
		}
		plain, err := d.crypto.Decrypt(ct)
		if err != nil {
			// A key we cannot decrypt is unusable (rotated master key, corrupt
			// row). Skip it rather than failing the whole request.
			lastErr = fmt.Errorf("key %d: %w", p.ID, err)
			continue
		}
		p.Plaintext = plain
		return p, nil
	}
	if err := rows.Err(); err != nil {
		return KeyPick{}, err
	}
	if lastErr != nil {
		return KeyPick{}, lastErr
	}
	return KeyPick{}, ErrNotFound
}

// UpsertAccount inserts or updates an account, returning its id.
func (d *DB) UpsertAccount(ctx context.Context, a provider.Account) (int64, error) {
	if _, err := provider.ParseKind(string(a.Kind)); err != nil {
		return 0, err
	}
	var ch string
	if len(a.CustomHeaders) > 0 {
		b, err := json.Marshal(a.CustomHeaders)
		if err != nil {
			return 0, err
		}
		ch = string(b)
	}
	probe := a.ProbeDelay.Milliseconds()
	if probe <= 0 {
		probe = 400
	}
	var id int64
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO accounts (name, namespace, kind, base_url, enabled, requires_session_header,
                      quota_mode, custom_headers, probe_delay_ms)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(namespace) DO UPDATE SET
  name=excluded.name, kind=excluded.kind, base_url=excluded.base_url,
  enabled=excluded.enabled, requires_session_header=excluded.requires_session_header,
  custom_headers=excluded.custom_headers, probe_delay_ms=excluded.probe_delay_ms,
  updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
			a.Name, a.Namespace, string(a.Kind), a.BaseURL, boolToInt(a.Enabled),
			boolToInt(a.RequiresSessionHeader), orDefault(a.QuotaMode, "probe"), ch, probe)
		if err != nil {
			return err
		}
		// LastInsertId is unreliable on the ON CONFLICT DO UPDATE path (SQLite
		// reports 0 when no new row was created), so always resolve the id by
		// its unique namespace. Boot seeding hit this as "store: not found".
		return tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE namespace=?`, a.Namespace).Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// AddKey encrypts and stores a provider key. Callers MUST have tested it first
// (the registration flow does); this only persists.
func (d *DB) AddKey(ctx context.Context, accountID int64, label, plaintext string) (int64, string, error) {
	if strings.TrimSpace(plaintext) == "" {
		return 0, "", errors.New("store: empty api key")
	}
	ct, err := d.crypto.Encrypt(plaintext)
	if err != nil {
		return 0, "", err
	}
	hint := KeyHint(plaintext)
	var id int64
	err = d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
INSERT INTO provider_keys (account_id, label, key_hint, key_ct, enabled)
VALUES (?,?,?,?,1)`, accountID, label, hint, ct)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, hint, err
}

// ── models ──────────────────────────────────────────────────────────────────

// UpsertModels replaces an account's catalog snapshot.
func (d *DB) UpsertModels(ctx context.Context, accountID int64, models []provider.ModelInfo) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM models WHERE account_id=?`, accountID); err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, `
INSERT INTO models (account_id, model_id, display_name, owned_by) VALUES (?,?,?,?)
ON CONFLICT(account_id, model_id) DO UPDATE SET
  display_name=excluded.display_name, owned_by=excluded.owned_by`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, m := range models {
			if m.ID == "" {
				continue
			}
			if _, err := stmt.ExecContext(ctx, accountID, m.ID, m.Display, m.OwnedBy); err != nil {
				return err
			}
		}
		return nil
	})
}

// ModelExists reports whether an account's catalog contains a model.
// An empty catalog (never synced) returns true so a fresh account still routes
// — the upstream is authoritative and will reject an unknown model itself.
func (d *DB) ModelExists(ctx context.Context, accountID int64, modelID string) (bool, error) {
	var total int
	if err := d.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM models WHERE account_id=?`, accountID).Scan(&total); err != nil {
		return false, err
	}
	if total == 0 {
		return true, nil
	}
	var n int
	if err := d.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM models WHERE account_id=? AND model_id=?`, accountID, modelID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// AllNamespaces lists every "<namespace>/<model>" pair, for helpful 404s and
// for /v1/models. Accounts with an unsynced catalog appear bare.
func (d *DB) AllNamespaces(ctx context.Context) ([]string, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT a.namespace, m.model_id
FROM accounts a
LEFT JOIN models m ON m.account_id = a.id
WHERE a.enabled = 1
ORDER BY a.namespace, m.model_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var ns string
		var model sql.NullString
		if err := rows.Scan(&ns, &model); err != nil {
			return nil, err
		}
		if model.Valid && model.String != "" {
			out = append(out, ns+"/"+model.String)
		} else {
			out = append(out, ns+"/…")
		}
	}
	return out, rows.Err()
}

// ModelEntry is one /v1/models row.
type ModelEntry struct {
	ID       string `json:"id"`
	OwnedBy  string `json:"owned_by"`
	Created  int64  `json:"created"`
	Object   string `json:"object"`
	Protocol struct {
		OpenAI    *bool `json:"openai,omitempty"`
		Anthropic *bool `json:"anthropic,omitempty"`
		Responses *bool `json:"responses,omitempty"`
	} `json:"protocol,omitempty"`
}

// ListModelEntries returns the catalog as "<namespace>/<model>" ids for /v1/models.
func (d *DB) ListModelEntries(ctx context.Context) ([]ModelEntry, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT a.namespace, m.model_id, COALESCE(m.owned_by,''), COALESCE(m.display_name,''),
       m.proto_openai, m.proto_anthropic, m.proto_responses
FROM accounts a
JOIN models m ON m.account_id = a.id
WHERE a.enabled = 1
ORDER BY a.namespace, m.model_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelEntry{}
	for rows.Next() {
		var ns, model, owned, display string
		var po, pa, pr sql.NullInt64
		if err := rows.Scan(&ns, &model, &owned, &display, &po, &pa, &pr); err != nil {
			return nil, err
		}
		e := ModelEntry{ID: ns + "/" + model, OwnedBy: ns, Object: "model"}
		if owned != "" {
			e.OwnedBy = owned
		}
		e.Protocol.OpenAI = nullBoolPtr(po)
		e.Protocol.Anthropic = nullBoolPtr(pa)
		e.Protocol.Responses = nullBoolPtr(pr)
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullBoolPtr(n sql.NullInt64) *bool {
	if !n.Valid {
		return nil
	}
	b := n.Int64 == 1
	return &b
}

// ── client tokens ───────────────────────────────────────────────────────────

// ClientByToken resolves a bearer token to its client name + roles.
// Tokens are stored hashed; the plaintext is never persisted.
func (d *DB) ClientByToken(ctx context.Context, token string) (name string, roles []string, err error) {
	if token == "" {
		return "", nil, ErrNotFound
	}
	h := HashToken(token)
	var rolesCSV string
	var enabled int
	err = d.r.QueryRowContext(ctx,
		`SELECT name, roles, enabled FROM client_tokens WHERE token_hash = ?`, h).
		Scan(&name, &rolesCSV, &enabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, ErrNotFound
		}
		return "", nil, err
	}
	if enabled != 1 {
		return "", nil, ErrNotFound
	}
	return name, strings.Split(rolesCSV, ","), nil
}

// AddClientToken stores a new client token (hashed). Returns the id.
func (d *DB) AddClientToken(ctx context.Context, name, plaintextToken, roles string) (int64, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(plaintextToken) == "" {
		return 0, errors.New("store: name and token are required")
	}
	if roles == "" {
		roles = "infer"
	}
	var id int64
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO client_tokens (name, token_hash, roles, enabled) VALUES (?,?,?,1)`,
			name, HashToken(plaintextToken), roles)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// HasKeyByHint reports whether an account already stores a key with this hint.
// Used by seeding so a restart never duplicates rows (we compare hints because
// the plaintext is not available after encryption).
func (d *DB) HasKeyByHint(ctx context.Context, accountID int64, hint string) (bool, error) {
	var n int
	err := d.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM provider_keys WHERE account_id=? AND key_hint=?`, accountID, hint).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpsertClientToken inserts or replaces a client token (hashed). Idempotent by
// name, so re-seeding on boot never duplicates.
func (d *DB) UpsertClientToken(ctx context.Context, name, plaintextToken, roles string) error {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(plaintextToken) == "" {
		return errors.New("store: name and token are required")
	}
	if roles == "" {
		roles = "infer"
	}
	hash := HashToken(plaintextToken)
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		// token_hash is UNIQUE alongside name, so ON CONFLICT(name) alone is not
		// enough: when the incoming plaintext already belongs to a DIFFERENT row
		// (token not rotated, or two config names seeded the same value) the hash
		// index rejects the write BEFORE ON CONFLICT(name) can apply, and boot
		// died with "UNIQUE constraint failed: client_tokens.token_hash".
		//
		// Step 1 releases the hash. It is only ever held by a row whose OWN name
		// is not `name` — that row's token is being reassigned to `name`, so it
		// is unreachable by definition and safe to delete. Nothing else is
		// touched: an unrelated row's token keeps its name and its row.
		// (Doing this as a DELETE rather than a rename avoids inventing a
		// placeholder name that could itself collide, and a UNIQUE index has no
		// deferred mode to lean on.)
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM client_tokens WHERE token_hash = ? AND name <> ?`,
			hash, name); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO client_tokens (name, token_hash, roles, enabled) VALUES (?,?,?,1)
ON CONFLICT(name) DO UPDATE SET
  token_hash=excluded.token_hash, roles=excluded.roles, enabled=1`,
			name, hash, roles)
		return err
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// orDefault returns v, or def when v is empty.
func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
