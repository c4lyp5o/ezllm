package store

// M3 admin store layer: account/key CRUD, protocol-verdict persistence,
// quota snapshots, combos, client tokens, and the /admin/export dump.
//
// Conventions inherited from catalog.go:
//   - single writer (WithWriteTx), reads through the pool
//   - ON CONFLICT paths resolve ids by unique key (LastInsertId lies there)
//   - provider_keys rows are only ever written by the server AFTER a passing
//     key test; nothing here invents an untested credential

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// ── accounts ────────────────────────────────────────────────────────────────

// AccountSummary is the admin-API shape: an account plus its keys, latest
// quota and protocol counts. Mirrors docs/API.md.
type AccountSummary struct {
	ID              int64          `json:"id"`
	Name            string         `json:"name"`
	Namespace       string         `json:"namespace"`
	Kind            string         `json:"kind"`
	BaseURL         string         `json:"base_url"`
	Enabled         bool           `json:"enabled"`
	RequiresSession bool           `json:"requires_session_header"`
	QuotaMode       string         `json:"quota_mode"`
	CapWindow       string         `json:"cap_window"`
	CapTokens       int64          `json:"cap_tokens"`
	ProbeDelayMS    int            `json:"probe_delay_ms"`
	Notes           string         `json:"notes,omitempty"`
	CreatedAt       string         `json:"created_at"`
	UpdatedAt       string         `json:"updated_at"`
	Keys            []KeySummary   `json:"keys"`
	Quota           *QuotaSummary  `json:"quota"`
	ModelsCount     int            `json:"models_count"`
	ProtocolSupport map[string]int `json:"protocol_support"`
}

// KeySummary is the only surviving shape of a credential: hint + test history.
type KeySummary struct {
	ID             int64  `json:"id"`
	Label          string `json:"label"`
	Hint           string `json:"hint"`
	Enabled        bool   `json:"enabled"`
	LastTestAt     string `json:"last_test_at,omitempty"`
	LastTestOK     *bool  `json:"last_test_ok"`
	LastTestDetail string `json:"last_test_detail,omitempty"`
	AddedAt        string `json:"added_at"`
}

// QuotaSummary is the latest quota snapshot for an account's most-recently
// tested key. Kind decides which fields matter (smart-adapter rendering).
type QuotaSummary struct {
	Kind           string   `json:"kind"`
	PercentRolling *float64 `json:"percent_rolling"`
	PercentWeekly  *float64 `json:"percent_weekly"`
	PercentMonthly *float64 `json:"percent_monthly"`
	Balance        *float64 `json:"balance"`
	Unit           *string  `json:"unit"`
	CostTotal      *float64 `json:"cost_total"`
	CostToday      *float64 `json:"cost_today"`
	ResetsAt       *string  `json:"resets_at"`
	ReadAt         string   `json:"read_at"`
}

// ListAccounts returns every account with keys, latest quota and protocol
// counts — one query set so the providers page needs no N+1 fan-out.
func (d *DB) ListAccounts(ctx context.Context) ([]AccountSummary, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT id, name, namespace, kind, base_url, enabled, requires_session_header,
       quota_mode, cap_window, cap_tokens, COALESCE(probe_delay_ms,400),
       COALESCE(notes,''), created_at, updated_at
FROM accounts ORDER BY namespace`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AccountSummary{}
	for rows.Next() {
		var a AccountSummary
		if err := rows.Scan(&a.ID, &a.Name, &a.Namespace, &a.Kind, &a.BaseURL, &a.Enabled,
			&a.RequiresSession, &a.QuotaMode, &a.CapWindow, &a.CapTokens,
			&a.ProbeDelayMS, &a.Notes, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := d.hydrateAccount(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetAccount is ListAccounts filtered to one id.
func (d *DB) GetAccount(ctx context.Context, id int64) (*AccountSummary, error) {
	all, err := d.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, ErrNotFound
}

// hydrateAccount fills keys / quota / protocol counts for one account.
func (d *DB) hydrateAccount(ctx context.Context, a *AccountSummary) error {
	a.Keys = []KeySummary{} // API contract: collections are arrays, including when empty.
	a.ProtocolSupport = map[string]int{"openai": 0, "anthropic": 0, "responses": 0, "untested": 0}

	// keys
	krows, err := d.r.QueryContext(ctx, `
SELECT id, label, key_hint, enabled, last_test_at, last_test_ok,
       COALESCE(last_test_detail,''), added_at
FROM provider_keys WHERE account_id=? ORDER BY id`, a.ID)
	if err != nil {
		return err
	}
	defer krows.Close()
	for krows.Next() {
		var k KeySummary
		var lastTest sql.NullString
		var ok sql.NullBool
		if err := krows.Scan(&k.ID, &k.Label, &k.Hint, &k.Enabled, &lastTest, &ok,
			&k.LastTestDetail, &k.AddedAt); err != nil {
			return err
		}
		k.LastTestAt = lastTest.String
		if ok.Valid {
			v := ok.Bool
			k.LastTestOK = &v
		}
		a.Keys = append(a.Keys, k)
	}
	if err := krows.Err(); err != nil {
		return err
	}

	// latest quota across this account's keys (most recent read wins)
	var q QuotaSummary
	var qReadAt string
	err = d.r.QueryRowContext(ctx, `
SELECT q.kind, q.percent_rolling, q.percent_weekly, q.percent_monthly,
       q.balance, q.balance_unit, q.cost_total, q.cost_today,
       q.resets_at, q.read_at
FROM quota_snapshots q
JOIN provider_keys k ON k.id = q.provider_key_id
WHERE k.account_id = ?
ORDER BY q.read_at DESC, q.id DESC LIMIT 1`, a.ID).
		Scan(&q.Kind, &q.PercentRolling, &q.PercentWeekly, &q.PercentMonthly,
			&q.Balance, &q.Unit, &q.CostTotal, &q.CostToday, &q.ResetsAt, &qReadAt)
	if err == nil {
		q.ReadAt = qReadAt
		a.Quota = &q
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// model counts + protocol support (map values can't be addressed, so scan
	// into locals and place them)
	var openai, anthropic, responses, untested int
	err = d.r.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN proto_openai=1 THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN proto_anthropic=1 THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN proto_responses=1 THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN proto_openai IS NULL AND proto_anthropic IS NULL
                          AND proto_responses IS NULL THEN 1 ELSE 0 END),0)
FROM models WHERE account_id=?`, a.ID).
		Scan(&a.ModelsCount, &openai, &anthropic, &responses, &untested)
	if err != nil {
		return err
	}
	a.ProtocolSupport["openai"] = openai
	a.ProtocolSupport["anthropic"] = anthropic
	a.ProtocolSupport["responses"] = responses
	a.ProtocolSupport["untested"] = untested
	return nil
}

// AccountInput is the create/update payload from the admin API.
type AccountInput struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Kind            string            `json:"kind"`
	BaseURL         string            `json:"base_url"`
	Enabled         *bool             `json:"enabled"`
	RequiresSession *bool             `json:"requires_session_header"`
	QuotaMode       string            `json:"quota_mode"`
	CustomHeaders   map[string]string `json:"custom_headers"`
	ProbeDelayMS    int               `json:"probe_delay_ms"`
	CapWindow       string            `json:"cap_window"`
	CapTokens       *int64            `json:"cap_tokens"`
	Notes           string            `json:"notes"`
}

// ErrConflict marks a uniqueness clash (409): namespace or name taken, or a
// referenced row that must not be deleted.
type ErrConflict struct {
	What    string
	Details map[string]any
}

func (e *ErrConflict) Error() string { return "store: conflict: " + e.What }

var namespaceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidateAccountInput enforces the API contract before any write.
func ValidateAccountInput(in AccountInput) error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("name is required")
	}
	ns := strings.TrimSpace(in.Namespace)
	if ns == "" {
		return errors.New("namespace is required")
	}
	if strings.Contains(ns, "/") {
		return errors.New("namespace must not contain '/' (combos are addressed by bare name; '/' separates namespace from model)")
	}
	if !namespaceRe.MatchString(ns) {
		return errors.New("namespace must be a lowercase slug: a-z, 0-9, dot, dash, underscore (max 63)")
	}
	if _, err := provider.ParseKind(in.Kind); err != nil {
		return err
	}
	if in.Kind == string(provider.KindOpenCodeGo) && strings.TrimSpace(in.BaseURL) == "" {
		in.BaseURL = "https://opencode.ai/zen/go/v1"
	}
	if strings.TrimSpace(in.BaseURL) == "" {
		return errors.New("base_url is required for compatibility providers")
	}
	if !strings.HasPrefix(in.BaseURL, "http://") && !strings.HasPrefix(in.BaseURL, "https://") {
		return errors.New("base_url must start with http:// or https://")
	}
	if in.QuotaMode != "" {
		switch in.QuotaMode {
		case "probe", "percent", "money", "tokens", "none":
		default:
			return errors.New("quota_mode must be probe|percent|money|tokens|none")
		}
	}
	if in.CapWindow != "" {
		switch in.CapWindow {
		case "5h", "daily", "weekly", "monthly":
		default:
			return errors.New("cap_window must be 5h|daily|weekly|monthly")
		}
	}
	return nil
}

// CreateAccount inserts a new account and returns its id. Conflicts →
// *ErrConflict (the server maps it to 409).
func (d *DB) CreateAccount(ctx context.Context, in AccountInput) (int64, error) {
	if in.Kind == string(provider.KindOpenCodeGo) && strings.TrimSpace(in.BaseURL) == "" {
		in.BaseURL = "https://opencode.ai/zen/go/v1"
	}
	if err := ValidateAccountInput(in); err != nil {
		return 0, err
	}
	kind, err := provider.ParseKind(in.Kind)
	if err != nil {
		return 0, err
	}
	id, err := d.writeAccount(ctx, 0, in, kind)
	if err != nil && isUniqueViolation(err) {
		return 0, &ErrConflict{What: accountConflictMessage(ctx, d, in)}
	}
	return id, err
}

func accountConflictMessage(ctx context.Context, d *DB, in AccountInput) string {
	var nameExists, namespaceExists int
	if err := d.w.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE name = ?),
		EXISTS(SELECT 1 FROM accounts WHERE namespace = ?)`, in.Name, in.Namespace).Scan(&nameExists, &namespaceExists); err != nil {
		return "account name or namespace already in use"
	}
	switch {
	case nameExists != 0 && namespaceExists != 0:
		return "account name and namespace already in use"
	case nameExists != 0:
		return "account name already in use"
	case namespaceExists != 0:
		return "account namespace already in use"
	default:
		return "account name or namespace already in use"
	}
}

// UpdateAccount patches an existing account (partial update). Namespace changes
// re-point routing, which is why it is allowed but uniqueness-checked.
func (d *DB) UpdateAccount(ctx context.Context, id int64, in AccountInput) error {
	cur, err := d.AccountByID(ctx, id)
	if err != nil {
		return err
	}
	// Fill gaps with current values so a partial PATCH never blanks a field.
	if in.Name == "" {
		in.Name = cur.Name
	}
	if in.Namespace == "" {
		in.Namespace = cur.Namespace
	}
	if in.Kind == "" {
		in.Kind = string(cur.Kind)
	}
	if in.BaseURL == "" {
		in.BaseURL = cur.BaseURL
	}
	if in.QuotaMode == "" {
		in.QuotaMode = cur.QuotaMode
	}
	if in.CapWindow == "" {
		in.CapWindow = cur.CapWindow
	}
	if in.ProbeDelayMS == 0 {
		in.ProbeDelayMS = int(cur.ProbeDelay.Milliseconds())
		if in.ProbeDelayMS == 0 {
			in.ProbeDelayMS = 400
		}
	}
	// enabled/requires_session: nil means "leave as-is".
	if in.Enabled == nil {
		v := cur.Enabled
		in.Enabled = &v
	}
	if in.RequiresSession == nil {
		v := cur.RequiresSessionHeader
		in.RequiresSession = &v
	}
	if in.Notes == "" {
		in.Notes = cur.Notes
	}
	kind, err := provider.ParseKind(in.Kind)
	if err != nil {
		return err
	}
	if _, err := d.writeAccount(ctx, id, in, kind); err != nil && isUniqueViolation(err) {
		return &ErrConflict{What: "namespace or name already in use"}
	}
	return nil
}

func (d *DB) writeAccount(ctx context.Context, id int64, in AccountInput, kind provider.Kind) (int64, error) {
	var ch string
	if len(in.CustomHeaders) > 0 {
		b, err := json.Marshal(in.CustomHeaders)
		if err != nil {
			return 0, err
		}
		ch = string(b)
	}
	probe := in.ProbeDelayMS
	if probe <= 0 {
		probe = 400
	}
	quotaMode := in.QuotaMode
	if quotaMode == "" {
		quotaMode = "probe"
	}
	capWindow := in.CapWindow
	if capWindow == "" {
		capWindow = "monthly"
	}
	enabled, requires := 1, 0
	if in.Enabled != nil && !*in.Enabled {
		enabled = 0
	}
	if in.RequiresSession != nil && *in.RequiresSession {
		requires = 1
	}
	var capTokens int64
	if in.CapTokens != nil {
		capTokens = *in.CapTokens
	}

	var newID int64
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if id == 0 {
			res, err := tx.ExecContext(ctx, `
INSERT INTO accounts (name, namespace, kind, base_url, enabled, requires_session_header,
                      quota_mode, custom_headers, probe_delay_ms, cap_window, cap_tokens, notes)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
				in.Name, in.Namespace, string(kind), in.BaseURL, enabled, requires,
				quotaMode, ch, probe, capWindow, capTokens, in.Notes)
			if err != nil {
				return err
			}
			newID, err = res.LastInsertId()
			return err
		}
		_, err := tx.ExecContext(ctx, `
UPDATE accounts SET name=?, namespace=?, kind=?, base_url=?, enabled=?,
       requires_session_header=?, quota_mode=?, custom_headers=?, probe_delay_ms=?,
       cap_window=?, cap_tokens=?, notes=?,
       updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
WHERE id=?`,
			in.Name, in.Namespace, string(kind), in.BaseURL, enabled, requires,
			quotaMode, ch, probe, capWindow, capTokens, in.Notes, id)
		if err != nil {
			return err
		}
		newID = id
		return nil
	})
	return newID, err
}

// DeleteAccount removes an account. When combos still reference it and force
// is false, returns *ErrConflict listing them (the server maps it to 409).
func (d *DB) DeleteAccount(ctx context.Context, id int64, force bool) error {
	if !force {
		rows, err := d.r.QueryContext(ctx, `
SELECT c.id, c.name FROM combos c
JOIN combo_hops h ON h.combo_id = c.id
WHERE h.account_id = ? GROUP BY c.id, c.name`, id)
		if err != nil {
			return err
		}
		var refs []map[string]any
		for rows.Next() {
			var cid int64
			var name string
			if err := rows.Scan(&cid, &name); err != nil {
				rows.Close()
				return err
			}
			refs = append(refs, map[string]any{"id": cid, "name": name})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(refs) > 0 {
			return &ErrConflict{
				What:    fmt.Sprintf("account referenced by %d combo(s)", len(refs)),
				Details: map[string]any{"combos": refs},
			}
		}
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		// combos referencing the account must go first when forcing: hops have
		// ON DELETE RESTRICT-style intent (no CHECK, but the UI needs the combo
		// to stay coherent). We delete the hops, leaving combos intact.
		if _, err := tx.ExecContext(ctx, `DELETE FROM combo_hops WHERE account_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM models WHERE account_id=?`, id); err != nil {
			return err
		}
		// quota_snapshots + provider_keys: cascade handles keys; snapshots
		// follow their key row (FK ON DELETE CASCADE), so delete keys last.
		res, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ── keys ────────────────────────────────────────────────────────────────────

// MarkKeyTest records a key test outcome (the server only calls this AFTER a
// pass on first-add; retests may record failures against a live row).
func (d *DB) MarkKeyTest(ctx context.Context, keyID int64, ok bool, detail string) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
UPDATE provider_keys SET last_test_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),
       last_test_ok=?, last_test_detail=? WHERE id=?`,
			boolToInt(ok), detail, keyID)
		return err
	})
}

// ListKeys returns an account's keys (hints only — plaintext never leaves
// the store/API boundary).
func (d *DB) ListKeys(ctx context.Context, accountID int64) ([]KeySummary, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT id, label, key_hint, enabled, last_test_at, last_test_ok,
       COALESCE(last_test_detail,''), added_at
FROM provider_keys WHERE account_id=? ORDER BY id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KeySummary{}
	for rows.Next() {
		var k KeySummary
		var lastTest sql.NullString
		var ok sql.NullBool
		if err := rows.Scan(&k.ID, &k.Label, &k.Hint, &k.Enabled, &lastTest, &ok,
			&k.LastTestDetail, &k.AddedAt); err != nil {
			return nil, err
		}
		k.LastTestAt = lastTest.String
		if ok.Valid {
			v := ok.Bool
			k.LastTestOK = &v
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// KeyForTest reads a stored key so a retest can run against the provider.
// The plaintext returns to memory only — never to a response body.
func (d *DB) KeyForTest(ctx context.Context, keyID int64) (accountID int64, plaintext string, err error) {
	row := d.r.QueryRowContext(ctx, `
SELECT account_id, key_plain FROM provider_keys WHERE id=?`, keyID)
	if err := row.Scan(&accountID, &plaintext); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", ErrNotFound
		}
		return 0, "", err
	}
	return accountID, plaintext, nil
}

// DeleteKey removes one credential.
func (d *DB) DeleteKey(ctx context.Context, accountID, keyID int64) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM provider_keys WHERE id=? AND account_id=?`, keyID, accountID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// EnableKey toggles a credential without deleting it.
func (d *DB) EnableKey(ctx context.Context, keyID int64, enabled bool) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE provider_keys SET enabled=? WHERE id=?`, boolToInt(enabled), keyID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ── models + protocol verdicts ──────────────────────────────────────────────

// ModelEntryRow is one catalog row with its three-valued protocol state.
type ModelEntryRow struct {
	ID        string  `json:"id"`
	Display   string  `json:"display,omitempty"`
	OwnedBy   string  `json:"owned_by,omitempty"`
	OpenAI    *bool   `json:"openai"`
	Anthropic *bool   `json:"anthropic"`
	Responses *bool   `json:"responses"`
	TestedAt  *string `json:"tested_at,omitempty"`
}

// ListModels returns an account's catalog with protocol verdicts.
func (d *DB) ListModels(ctx context.Context, accountID int64) ([]ModelEntryRow, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT model_id, COALESCE(display_name,''), COALESCE(owned_by,''),
       proto_openai, proto_anthropic, proto_responses, proto_tested_at
FROM models WHERE account_id=? ORDER BY model_id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelEntryRow{}
	for rows.Next() {
		var m ModelEntryRow
		var o, a, r sql.NullBool
		var tested sql.NullString
		if err := rows.Scan(&m.ID, &m.Display, &m.OwnedBy, &o, &a, &r, &tested); err != nil {
			return nil, err
		}
		if o.Valid {
			v := o.Bool
			m.OpenAI = &v
		}
		if a.Valid {
			v := a.Bool
			m.Anthropic = &v
		}
		if r.Valid {
			v := r.Bool
			m.Responses = &v
		}
		if tested.Valid && tested.String != "" {
			m.TestedAt = &tested.String
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetModelProto records one probe verdict. nil → NULL (untested), which is a
// first-class state: "we could not ask" must never render as "unsupported".
func (d *DB) SetModelProto(ctx context.Context, accountID int64, modelID string, s provider.Surface, supported *bool) error {
	col := "proto_openai"
	switch s {
	case provider.SurfaceAnthropic:
		col = "proto_anthropic"
	case provider.SurfaceResponses:
		col = "proto_responses"
	}
	// Upsert on (account_id, model_id): the sweep may probe a model that the
	// catalog sync has not inserted yet (or vice versa).
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO models (account_id, model_id, %s, proto_tested_at)
VALUES (?,?,?,strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'))
ON CONFLICT(account_id, model_id) DO UPDATE SET
  %s=excluded.%s, proto_tested_at=excluded.proto_tested_at`,
			col, col, col),
			accountID, modelID, boolPtrToInt(supported))
		return err
	})
}

// SetModelProtos applies a whole sweep in ONE transaction — the common case
// after a background protocol probe, and the reason the sweep does not take
// the write lock 108 times.
func (d *DB) SetModelProtos(ctx context.Context, accountID int64, rows map[string]map[string]*bool) error {
	if len(rows) == 0 {
		return nil
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		stmts := map[string]*sql.Stmt{}
		defer func() {
			for _, s := range stmts {
				s.Close()
			}
		}()
		get := func(col string) (*sql.Stmt, error) {
			if st, ok := stmts[col]; ok {
				return st, nil
			}
			st, err := tx.PrepareContext(ctx, fmt.Sprintf(`
INSERT INTO models (account_id, model_id, %s, proto_tested_at)
VALUES (?,?,?,strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'))
ON CONFLICT(account_id, model_id) DO UPDATE SET
  %s=excluded.%s, proto_tested_at=excluded.proto_tested_at`, col, col, col))
			if err != nil {
				return nil, err
			}
			stmts[col] = st
			return st, nil
		}
		for modelID, perSurface := range rows {
			for surf, val := range perSurface {
				col := "proto_openai"
				switch provider.Surface(surf) {
				case provider.SurfaceAnthropic:
					col = "proto_anthropic"
				case provider.SurfaceResponses:
					col = "proto_responses"
				}
				st, err := get(col)
				if err != nil {
					return err
				}
				if _, err := st.ExecContext(ctx, accountID, modelID, boolPtrToInt(val)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// ── quota snapshots (append-only) ───────────────────────────────────────────

// InsertQuotaSnapshot stores one immutable quota read.
func (d *DB) InsertQuotaSnapshot(ctx context.Context, keyID int64, q provider.Quota, ok bool, errMsg string) error {
	kind := string(q.Kind)
	if kind == "" {
		if ok {
			kind = "none"
		} else {
			kind = "error"
		}
	}
	resets := q.RollingResetsAt
	if resets == "" {
		resets = q.WeeklyResetsAt
	}
	if resets == "" {
		resets = q.MonthlyResetsAt
	}
	raw := q.Raw
	if raw == "" {
		raw = "{}"
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO quota_snapshots
  (provider_key_id, kind, percent_rolling, percent_weekly, percent_monthly,
   balance, balance_unit, cost_total, cost_today, resets_at, raw, ok, err)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			keyID, kind, q.PercentRolling, q.PercentWeekly, q.PercentMonthly,
			q.Balance, nullIfEmpty(q.BalanceUnit), q.CostTotal, q.CostToday,
			nullIfEmpty(resets), raw, boolToInt(ok), nullIfEmpty(errMsg))
		return err
	})
}

// LatestQuotas returns the newest snapshot per key (the poller's read side).
func (d *DB) LatestQuotas(ctx context.Context, keyIDs []int64) ([]map[string]any, error) {
	q := `
SELECT k.id, k.account_id, k.label, k.key_hint,
       -- LEFT JOIN: a never-probed key has all-NULL quota columns, and kind/
       -- read_at/ok scan into plain string/bool. No snapshot => '' / false.
       COALESCE(q.kind,''), q.percent_rolling, q.percent_weekly, q.percent_monthly,
       q.balance, q.balance_unit, q.cost_total, q.cost_today, q.resets_at,
       COALESCE(q.read_at,''), COALESCE(q.ok,0)
FROM provider_keys k
LEFT JOIN quota_snapshots q ON q.id = (
  SELECT id FROM quota_snapshots WHERE provider_key_id = k.id
  ORDER BY read_at DESC, id DESC LIMIT 1)`
	var args []any
	if len(keyIDs) > 0 {
		ph := make([]string, len(keyIDs))
		for i, id := range keyIDs {
			ph[i] = "?"
			args = append(args, id)
		}
		q += " WHERE k.id IN (" + strings.Join(ph, ",") + ")"
	}
	q += " ORDER BY k.account_id, k.id"

	rows, err := d.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var keyID, acctID int64
		var label, hint, kind, readAt string
		var pr, pw, pm, bal, ct, cd sql.NullFloat64
		var unit, resets sql.NullString
		var ok bool
		if err := rows.Scan(&keyID, &acctID, &label, &hint, &kind, &pr, &pw, &pm,
			&bal, &unit, &ct, &cd, &resets, &readAt, &ok); err != nil {
			return nil, err
		}
		item := map[string]any{
			"key_id": keyID, "account_id": acctID, "label": label, "hint": hint,
			"kind": kind, "read_at": readAt, "ok": ok,
			"percent_rolling": nullFloat(pr), "percent_weekly": nullFloat(pw),
			"percent_monthly": nullFloat(pm), "balance": nullFloat(bal),
			"unit": nullStr(unit), "cost_total": nullFloat(ct),
			"cost_today": nullFloat(cd), "resets_at": nullStr(resets),
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ── combos ──────────────────────────────────────────────────────────────────

// ComboHop is one ordered hop in a combo. Combos have NO namespace — they are
// addressed by bare name, and they do the hopping between explicit hops.
type ComboHop struct {
	AccountID int64  `json:"account_id"`
	ModelID   string `json:"model_id"`
	Weight    int    `json:"weight"`
	Enabled   *bool  `json:"enabled"`
}

// Combo is the admin-API combo shape.
type Combo struct {
	ID                   int64      `json:"id"`
	Name                 string     `json:"name"`
	Strategy             string     `json:"strategy"`
	StickyIdleS          int        `json:"sticky_idle_s"`
	Enabled              bool       `json:"enabled"`
	CompressionProfileID *int64     `json:"compression_profile_id"`
	Notes                string     `json:"notes,omitempty"`
	CreatedAt            string     `json:"created_at"`
	Hops                 []ComboHop `json:"hops"`
}

// ValidStrategies mirrors the schema CHECK — one source of truth for the UI.
func ValidStrategies() []string {
	return []string{"failover", "true_round_robin", "strict_round_robin", "sticky_last_good", "least_used"}
}

// ValidateCombo checks the payload before writing (422 on failure).
func ValidateCombo(c Combo) error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("name is required")
	}
	if strings.Contains(c.Name, "/") {
		return errors.New("combo name must not contain '/' — combos have no namespace")
	}
	valid := false
	for _, s := range ValidStrategies() {
		if c.Strategy == s {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("strategy must be one of %v", ValidStrategies())
	}
	if c.StickyIdleS < 0 {
		return errors.New("sticky_idle_s must be >= 0")
	}
	for i, h := range c.Hops {
		if h.AccountID <= 0 {
			return fmt.Errorf("hops[%d]: account_id is required", i)
		}
		if strings.TrimSpace(h.ModelID) == "" {
			return fmt.Errorf("hops[%d]: model_id is required", i)
		}
	}
	return nil
}

// ListCombos returns all combos with their ordered hops.
func (d *DB) ListCombos(ctx context.Context) ([]Combo, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT id, name, strategy, sticky_idle_s, enabled, compression_profile_id,
       COALESCE(notes,''), created_at
FROM combos ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Combo{}
	byID := map[int64]int{}
	for rows.Next() {
		var c Combo
		var cpid sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Name, &c.Strategy, &c.StickyIdleS, &c.Enabled,
			&cpid, &c.Notes, &c.CreatedAt); err != nil {
			return nil, err
		}
		if cpid.Valid {
			v := cpid.Int64
			c.CompressionProfileID = &v
		}
		c.Hops = []ComboHop{}
		byID[c.ID] = len(out)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hrows, err := d.r.QueryContext(ctx, `
SELECT combo_id, account_id, model_id, weight, enabled
FROM combo_hops ORDER BY combo_id, position`)
	if err != nil {
		return nil, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var comboID int64
		var h ComboHop
		if err := hrows.Scan(&comboID, &h.AccountID, &h.ModelID, &h.Weight, &h.Enabled); err != nil {
			return nil, err
		}
		if idx, ok := byID[comboID]; ok {
			out[idx].Hops = append(out[idx].Hops, h)
		}
	}
	return out, hrows.Err()
}

// UpsertCombo creates or replaces a combo by name (idempotent POST per the
// API contract). Hops replace the whole ordered list; positions = array order.
func (d *DB) UpsertCombo(ctx context.Context, c Combo) (int64, error) {
	if err := ValidateCombo(c); err != nil {
		return 0, err
	}
	var id int64
	err := d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var cpid any
		if c.CompressionProfileID != nil {
			cpid = *c.CompressionProfileID
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO combos (name, strategy, sticky_idle_s, enabled, compression_profile_id, notes)
VALUES (?,?,?,?,?,?)
ON CONFLICT(name) DO UPDATE SET
  strategy=excluded.strategy, sticky_idle_s=excluded.sticky_idle_s,
  enabled=excluded.enabled, compression_profile_id=excluded.compression_profile_id,
  notes=excluded.notes`,
			c.Name, c.Strategy, c.StickyIdleS, boolToInt(c.Enabled), cpid, c.Notes)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM combos WHERE name=?`, c.Name).Scan(&id); err != nil {
			return err
		}
		return d.replaceHops(ctx, tx, id, c.Hops)
	})
	return id, err
}

// ReplaceHops rewrites a combo's ordered hop list (POST /admin/combos/{id}/hops).
func (d *DB) ReplaceHops(ctx context.Context, comboID int64, hops []ComboHop) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM combos WHERE id=?`, comboID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		return d.replaceHops(ctx, tx, comboID, hops)
	})
}

func (d *DB) replaceHops(ctx context.Context, tx *sql.Tx, comboID int64, hops []ComboHop) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM combo_hops WHERE combo_id=?`, comboID); err != nil {
		return err
	}
	for i, h := range hops {
		// A hop pointing at a missing account would just vanish at runtime —
		// reject it at write time instead ("store: " prefix => 422 in writeStoreErr).
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM accounts WHERE id=?`, h.AccountID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("store: hops[%d]: account_id %d does not exist", i, h.AccountID)
		}
		enabled := true
		if h.Enabled != nil {
			enabled = *h.Enabled
		}
		weight := h.Weight
		if weight <= 0 {
			weight = 1
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO combo_hops (combo_id, position, account_id, model_id, weight, enabled)
VALUES (?,?,?,?,?,?)`, comboID, i, h.AccountID, h.ModelID, weight, boolToInt(enabled)); err != nil {
			return err
		}
	}
	return nil
}

// DeleteCombo removes a combo (hops cascade).
func (d *DB) DeleteCombo(ctx context.Context, id int64) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM combos WHERE id=?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// AllComboNames lists enabled combo names — used for 404 hints, so a typo'd
// combo name tells the user what combos DO exist.
func (d *DB) AllComboNames(ctx context.Context) ([]string, error) {
	rows, err := d.r.QueryContext(ctx, `SELECT name FROM combos WHERE enabled = 1 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ComboByName resolves a bare combo name (the router's combo entry point).
//
// This runs on the INFERENCE hot path — one query for the combo plus one for
// its hops, both indexed — rather than ListCombos, which pulls every combo and
// every hop in the database just to return one of them.
func (d *DB) ComboByName(ctx context.Context, name string) (*Combo, error) {
	row := d.r.QueryRowContext(ctx, `
SELECT id, name, strategy, sticky_idle_s, enabled, compression_profile_id,
       COALESCE(notes,''), created_at
FROM combos WHERE name = ?`, name)
	var c Combo
	var cpid sql.NullInt64
	if err := row.Scan(&c.ID, &c.Name, &c.Strategy, &c.StickyIdleS, &c.Enabled,
		&cpid, &c.Notes, &c.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if cpid.Valid {
		v := cpid.Int64
		c.CompressionProfileID = &v
	}
	c.Hops = []ComboHop{}

	hrows, err := d.r.QueryContext(ctx, `
SELECT account_id, model_id, weight, enabled
FROM combo_hops WHERE combo_id = ? ORDER BY position`, c.ID)
	if err != nil {
		return nil, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var h ComboHop
		if err := hrows.Scan(&h.AccountID, &h.ModelID, &h.Weight, &h.Enabled); err != nil {
			return nil, err
		}
		c.Hops = append(c.Hops, h)
	}
	return &c, hrows.Err()
}

// ── client tokens ───────────────────────────────────────────────────────────

// TokenSummary is the admin-API token shape (hash never leaves the store).
type TokenSummary struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Hint       string   `json:"hint"`
	Roles      []string `json:"roles"`
	Enabled    bool     `json:"enabled"`
	CapWindow  string   `json:"cap_window"`
	CapTokens  int64    `json:"cap_tokens"`
	LastUsedAt *string  `json:"last_used_at"`
	CreatedAt  string   `json:"created_at"`
}

// ListTokens returns all client tokens (hints only).
func (d *DB) ListTokens(ctx context.Context) ([]TokenSummary, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT id, name, token_hash, roles, enabled, cap_window, cap_tokens,
       -- no last_used_at column: derive from the ledger (calls.client is the
       -- token name, set by ClientByToken). No hot-path write needed.
       (SELECT MAX(ts) FROM calls c WHERE c.client = client_tokens.name),
       created_at
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
		if err := rows.Scan(&t.ID, &t.Name, &hash, &roles, &t.Enabled,
			&t.CapWindow, &t.CapTokens, &lastUsed, &t.CreatedAt); err != nil {
			return nil, err
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
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
	Token string   `json:"token"`
}

// ValidateToken checks roles before writing.
func (in TokenInput) Validate() error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("name is required")
	}
	for _, r := range in.Roles {
		switch r {
		case "infer", "admin":
		default:
			return fmt.Errorf("role %q must be infer|admin", r)
		}
	}
	return nil
}

// UpsertToken creates a token (generating one when not supplied) or updates an
// existing row's roles by name. Returns the plaintext token — the ONLY moment
// it exists outside the caller's request.
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
	roles := in.Roles
	if len(roles) == 0 {
		roles = []string{"infer"}
	}
	err = d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO client_tokens (name, token_hash, roles, enabled) VALUES (?,?,?,1)
ON CONFLICT(name) DO UPDATE SET roles=excluded.roles`,
			in.Name, HashToken(plaintext), strings.Join(roles, ","))
		return err
	})
	if err != nil {
		return 0, "", err
	}
	err = d.r.QueryRowContext(ctx, `SELECT id FROM client_tokens WHERE name=?`, in.Name).Scan(&id)
	return id, plaintext, err
}

// UpdateToken patches enabled/roles/caps.
func (d *DB) UpdateToken(ctx context.Context, id int64, enabled *bool, roles []string, capTokens *int64, capWindow string) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if enabled != nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE client_tokens SET enabled=? WHERE id=?`, boolToInt(*enabled), id); err != nil {
				return err
			}
		}
		if len(roles) > 0 {
			for _, r := range roles {
				switch r {
				case "infer", "admin":
				default:
					return fmt.Errorf("role %q must be infer|admin", r)
				}
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE client_tokens SET roles=? WHERE id=?`, strings.Join(roles, ","), id); err != nil {
				return err
			}
		}
		if capTokens != nil {
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
// gates (docs/compression-design.md). CRUD lives in compression.go.
type CompressionProfile struct {
	ID                int64           `json:"id"`
	Name              string          `json:"name"`
	Enabled           bool            `json:"enabled"`
	Stages            json.RawMessage `json:"stages"`
	ExemptLastTurn    bool            `json:"exempt_last_turn"`
	MinCompressRatio  float64         `json:"min_compress_ratio"`
	FailOpen          bool            `json:"fail_open"`
	AutoTriggerTokens int64           `json:"auto_trigger_tokens"`
	Notes             string          `json:"notes,omitempty"`
	CreatedAt         string          `json:"created_at,omitempty"`
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

// ── export ──────────────────────────────────────────────────────────────────

// Export is the disaster-recovery dump: everything needed to re-seed a fresh
// instance EXCEPT credentials (hints only — key_plain is deliberately absent).
type Export struct {
	ExportedAt    string               `json:"exported_at"`
	SchemaVersion int                  `json:"schema_version"`
	Accounts      []AccountSummary     `json:"accounts"`
	Combos        []Combo              `json:"combos"`
	Tokens        []TokenSummary       `json:"tokens"`
	Compression   []CompressionProfile `json:"compression_profiles"`
	Quotas        []map[string]any     `json:"latest_quotas"`
	Ledger        []map[string]any     `json:"ledger,omitempty"`
}

// ExportAll builds the dump. includeLedger adds the call history (potentially
// large), which is why it is opt-in.
func (d *DB) ExportAll(ctx context.Context, includeLedger bool) (*Export, error) {
	accounts, err := d.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	combos, err := d.ListCombos(ctx)
	if err != nil {
		return nil, err
	}
	tokens, err := d.ListTokens(ctx)
	if err != nil {
		return nil, err
	}
	profiles, err := d.ListCompressionProfiles(ctx)
	if err != nil {
		return nil, err
	}
	quotas, err := d.LatestQuotas(ctx, nil)
	if err != nil {
		return nil, err
	}
	exp := &Export{
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
		SchemaVersion: d.SchemaVersion(),
		Accounts:      accounts,
		Combos:        combos,
		Tokens:        tokens,
		Compression:   profiles,
		Quotas:        quotas,
	}
	if includeLedger {
		rows, err := d.r.QueryContext(ctx, `
SELECT ts, client, surface, alias, account, model, status, stream,
       ttft_ms, total_ms, tokens_in, tokens_out, tokens_cached_read,
       tokens_cached_write, reasoning_tokens, status
FROM calls ORDER BY id DESC LIMIT 10000`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		exp.Ledger = []map[string]any{}
		for rows.Next() {
			var ts, client, surface, alias, account, model string
			var status, stream int
			var ttft, total, tin, tout, cread, cwrite, reasoning sql.NullInt64
			var statusDup int
			if err := rows.Scan(&ts, &client, &surface, &alias, &account, &model, &status,
				&stream, &ttft, &total, &tin, &tout, &cread, &cwrite, &reasoning, &statusDup); err != nil {
				return nil, err
			}
			exp.Ledger = append(exp.Ledger, map[string]any{
				"ts": ts, "client": client, "surface": surface, "alias": alias,
				"account": account, "model": model, "status": status, "stream": stream == 1,
				"ttft_ms": nullInt(ttft), "total_ms": nullInt(total),
				"tokens_in": nullInt(tin), "tokens_out": nullInt(tout),
				"cached_read": nullInt(cread), "cached_write": nullInt(cwrite),
				"reasoning": nullInt(reasoning),
			})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return exp, nil
}

// ── shared helpers ──────────────────────────────────────────────────────────

func boolPtrToInt(p *bool) any {
	if p == nil {
		return nil // NULL = untested
	}
	return boolToInt(*p)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullFloat(v sql.NullFloat64) any {
	if !v.Valid {
		return nil
	}
	return v.Float64
}

func nullStr(v sql.NullString) any {
	if !v.Valid || v.String == "" {
		return nil
	}
	return v.String
}

func nullInt(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// tokenHint shows the last 4 hex chars of the hash — enough for a human to
// tell tokens apart, useless to an attacker.
func tokenHint(hash string) string {
	if len(hash) <= 4 {
		return "…"
	}
	return "…" + hash[len(hash)-4:]
}

// isUniqueViolation matches SQLite's UNIQUE/NOT NULL constraint text without
// depending on driver-specific error types.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed")
}
