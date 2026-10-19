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
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	Kind            string `json:"kind"`
	BaseURL         string `json:"base_url"`
	Enabled         bool   `json:"enabled"`
	RequiresSession bool   `json:"requires_session_header"`
	QuotaMode       string `json:"quota_mode"`
	CapWindow       string `json:"cap_window"`
	CapTokens       int64  `json:"cap_tokens"`
	// UsageConvention is the operator-declared input_tokens accounting of the
	// upstream Messages wire: auto (unknown) | anthropic | total.
	UsageConvention provider.UsageConvention `json:"usage_convention"`
	// RateLimit/RateWindow express the account's request ceiling as the
	// provider states it ("5 requests per 1 minute"), not as a token budget:
	// five calls to five different models spend the same 5/min tier, which no
	// per-model cap can express.
	RateLimit        int64          `json:"rate_limit"`
	RateWindow       string         `json:"rate_window"`
	ProbeDelayMS     int            `json:"probe_delay_ms"`
	Notes            string         `json:"notes,omitempty"`
	ManagementKeySet bool           `json:"management_key_set"`
	CreatedAt        string         `json:"created_at"`
	UpdatedAt        string         `json:"updated_at"`
	Keys             []KeySummary   `json:"keys"`
	Quota            *QuotaSummary  `json:"quota"`
	ModelsCount      int            `json:"models_count"`
	ProtocolSupport  map[string]int `json:"protocol_support"`
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
	TokensToday    *int64   `json:"tokens_today"`
	RequestsToday  *int64   `json:"requests_today"`
	ResetsAt       *string  `json:"resets_at"`
	ReadAt         string   `json:"read_at"`
}

// ListAccounts returns every account with keys, latest quota and protocol
// counts — one query set so the providers page needs no N+1 fan-out.
func (d *DB) ListAccounts(ctx context.Context) ([]AccountSummary, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT id, name, namespace, kind, base_url, enabled, requires_session_header,
       quota_mode, cap_window, cap_tokens, COALESCE(probe_delay_ms,400),
       COALESCE(rate_limit,0), COALESCE(rate_window,''),
       COALESCE(notes,''), created_at, updated_at, COALESCE(usage_convention,'auto'),
       COALESCE(management_key_plain,'')
FROM accounts ORDER BY namespace`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AccountSummary{}
	for rows.Next() {
		var a AccountSummary
		var managementKey string
		if err := rows.Scan(&a.ID, &a.Name, &a.Namespace, &a.Kind, &a.BaseURL, &a.Enabled,
			&a.RequiresSession, &a.QuotaMode, &a.CapWindow, &a.CapTokens,
			&a.ProbeDelayMS, &a.RateLimit, &a.RateWindow,
			&a.Notes, &a.CreatedAt, &a.UpdatedAt, &a.UsageConvention, &managementKey); err != nil {
			return nil, err
		}
		a.ManagementKeySet = strings.TrimSpace(managementKey) != ""
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
	var tokToday, reqToday sql.NullInt64
	err = d.r.QueryRowContext(ctx, `
SELECT q.kind, q.percent_rolling, q.percent_weekly, q.percent_monthly,
       q.balance, q.balance_unit, q.cost_total, q.cost_today,
       q.tokens_today, q.requests_today,
       q.resets_at, q.read_at
FROM quota_snapshots q
JOIN provider_keys k ON k.id = q.provider_key_id
WHERE k.account_id = ?
ORDER BY q.read_at DESC, q.id DESC LIMIT 1`, a.ID).
		Scan(&q.Kind, &q.PercentRolling, &q.PercentWeekly, &q.PercentMonthly,
			&q.Balance, &q.Unit, &q.CostTotal, &q.CostToday,
			&tokToday, &reqToday,
			&q.ResetsAt, &qReadAt)
	if err == nil {
		q.ReadAt = qReadAt
		if tokToday.Valid {
			v := tokToday.Int64
			q.TokensToday = &v
		}
		if reqToday.Valid {
			v := reqToday.Int64
			q.RequestsToday = &v
		}
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
	// Account-wide request budget. RateLimit is a pointer so a PATCH can
	// distinguish "clear the budget" (0) from "leave it alone" (nil), matching
	// the CapTokens convention above.
	RateLimit  *int64 `json:"rate_limit"`
	RateWindow string `json:"rate_window"`
	Notes      string `json:"notes"`
	// UsageConvention: '' = keep current (PATCH) / default 'auto' (create);
	// otherwise auto|anthropic|total. See provider.UsageConvention.
	UsageConvention string `json:"usage_convention"`
	// ManagementKey is an optional second credential for providers whose
	// balance endpoint needs a management key (TokenRouter). Empty on PATCH
	// keeps the stored value; the account list only reports whether it is set.
	ManagementKey string `json:"management_key"`
	// MaxContextTokens (addendum #2/#6): the operator-declared context window.
	// Requests whose estimated token count exceeds it are filtered before
	// dispatch. 0 = unknown/unlimited.
	MaxContextTokens *int64 `json:"max_context_tokens"`
}

// ErrConflict marks a uniqueness clash (409): namespace or name taken, or a
// referenced row that must not be deleted.
type ErrConflict struct {
	What    string
	Details map[string]any
}

func (e *ErrConflict) Error() string { return "store: conflict: " + e.What }

var namespaceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// bridgeSentinelFor maps a bridge kind to the sentinel base its runbook
// documents (docs/claude-web-runbook.md, docs/gemini-web-runbook.md).
func bridgeSentinelFor(kind string) (string, bool) {
	switch kind {
	case string(provider.KindClaudeWeb):
		return "internal://claudeweb", true
	case string(provider.KindGeminiWeb):
		return "internal://geminiweb", true
	}
	return "", false
}

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
	// Bridge kinds (claude-web, gemini-web) never dial a host the operator
	// names — the adapter rewrites to the in-process bridge, so their base
	// URL is the sentinel the runbooks document, defaulted when empty and
	// required verbatim when present. github-copilot is NOT a bridge kind:
	// it dials https://api.githubcopilot.com like any other provider.
	if sentinel, ok := bridgeSentinelFor(in.Kind); ok {
		// Empty is legal here — CreateAccount defaults it to the sentinel
		// before the write (this validator takes AccountInput by value).
		if in.BaseURL != "" && in.BaseURL != sentinel {
			return fmt.Errorf("base_url for kind %s must be %s (the bridge sentinel)", in.Kind, sentinel)
		}
	} else {
		if strings.TrimSpace(in.BaseURL) == "" {
			return errors.New("base_url is required for compatibility providers")
		}
		if !strings.HasPrefix(in.BaseURL, "http://") && !strings.HasPrefix(in.BaseURL, "https://") {
			return errors.New("base_url must start with http:// or https://")
		}
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
	if err := validateRate(in.RateLimit, in.RateWindow); err != nil {
		return err
	}
	if in.UsageConvention != "" {
		if _, err := provider.ParseUsageConvention(in.UsageConvention); err != nil {
			return err
		}
	}
	return nil
}

// validateRate enforces the request-budget contract.
//
// Both halves or neither: a count with no window has no meaning, and a window
// with no count is a limit of zero, which would refuse EVERY request. Refusing
// the ambiguous pair at write time is the only way to keep those two mistakes
// from becoming a silently dead or silently unlimited account.
func validateRate(limit *int64, window string) error {
	if window != "" {
		d, err := time.ParseDuration(window)
		if err != nil {
			return errors.New("rate_window must be a duration like 60s, 5m, 1h")
		}
		if d < time.Second || d > 24*time.Hour {
			return errors.New("rate_window must be between 1s and 24h")
		}
	}
	if limit == nil {
		return nil // window-only patch on an account that has no count yet
	}
	if *limit < 0 {
		return errors.New("rate_limit must be >= 0 (0 = unlimited)")
	}
	if *limit > 0 && window == "" {
		return errors.New("rate_limit requires rate_window (how long the count applies over)")
	}
	return nil
}

// CreateAccount inserts a new account and returns its id. Conflicts →
// *ErrConflict (the server maps it to 409).
func (d *DB) CreateAccount(ctx context.Context, in AccountInput) (int64, error) {
	if in.Kind == string(provider.KindOpenCodeGo) && strings.TrimSpace(in.BaseURL) == "" {
		in.BaseURL = "https://opencode.ai/zen/go/v1"
	}
	if sentinel, ok := bridgeSentinelFor(in.Kind); ok && strings.TrimSpace(in.BaseURL) == "" {
		in.BaseURL = sentinel
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
	// Rate budget: nil means "leave alone"; an explicit 0 clears it. The window
	// follows its count — with no count the window is meaningless, and carrying
	// a stale one would make a cleared account look configured.
	if in.RateLimit == nil {
		v := cur.RateLimit
		in.RateLimit = &v
		if in.RateWindow == "" && cur.RateWindow > 0 {
			in.RateWindow = cur.RateWindow.String()
		}
	}
	if *in.RateLimit == 0 {
		in.RateWindow = ""
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
	if in.UsageConvention == "" {
		in.UsageConvention = string(cur.UsageConvention)
	}
	if in.ManagementKey == "" {
		in.ManagementKey = cur.ManagementKey
	}
	kind, err := provider.ParseKind(in.Kind)
	if err != nil {
		return err
	}
	if _, err := d.writeAccount(ctx, id, in, kind); err != nil {
		if isUniqueViolation(err) {
			return &ErrConflict{What: "namespace or name already in use"}
		}
		return err
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
	usageConvention := in.UsageConvention
	if usageConvention == "" {
		usageConvention = string(provider.UsageConventionAuto)
	}
	parsedUsage, err := provider.ParseUsageConvention(usageConvention)
	if err != nil {
		return 0, err
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
	var rateLimit int64
	if in.RateLimit != nil {
		rateLimit = *in.RateLimit
	}
	var maxCtx int64
	if in.MaxContextTokens != nil {
		maxCtx = *in.MaxContextTokens
	}
	rateWindow := in.RateWindow
	if rateLimit <= 0 {
		// A count of zero means unlimited, so keep no window: a stored window
		// with no count is dead data that reads as "configured".
		rateWindow = ""
	}
	mgmtKey := in.ManagementKey
	if mgmtKey != "" && d.crypto != nil {
		enc, err := d.crypto.Encrypt(mgmtKey)
		if err != nil {
			return 0, err
		}
		mgmtKey = enc
	}

	var newID int64
	err = d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if id == 0 {
			res, err := tx.ExecContext(ctx, `
INSERT INTO accounts (name, namespace, kind, base_url, enabled, requires_session_header,
                      quota_mode, custom_headers, probe_delay_ms, cap_window, cap_tokens,
                      rate_limit, rate_window, notes, usage_convention, max_context_tokens,
                      management_key_plain)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				in.Name, in.Namespace, string(kind), in.BaseURL, enabled, requires,
				quotaMode, ch, probe, capWindow, capTokens, rateLimit, rateWindow, in.Notes,
				parsedUsage, maxCtx, mgmtKey)
			if err != nil {
				return err
			}
			newID, err = res.LastInsertId()
			return err
		}
		_, err := tx.ExecContext(ctx, `
UPDATE accounts SET name=?, namespace=?, kind=?, base_url=?, enabled=?,
       requires_session_header=?, quota_mode=?, custom_headers=?, probe_delay_ms=?,
       cap_window=?, cap_tokens=?, rate_limit=?, rate_window=?, notes=?,
       usage_convention=?, max_context_tokens=?, management_key_plain=?,
       updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
WHERE id=?`,
			in.Name, in.Namespace, string(kind), in.BaseURL, enabled, requires,
			quotaMode, ch, probe, capWindow, capTokens, rateLimit, rateWindow, in.Notes,
			parsedUsage, maxCtx, mgmtKey, id)
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
	if d.crypto != nil && strings.HasPrefix(plaintext, cryptoPrefix) {
		plaintext, err = d.crypto.Decrypt(plaintext)
		if err != nil {
			return 0, "", err
		}
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
	// Pin is the operator-chosen surface ("" = auto). Enforced by the router.
	Pin string `json:"pin"`
	// Source is who last wrote the verdicts: "probe", "curated", or "" for a
	// legacy row nobody has attributed yet. Curated rows survive probe sweeps.
	Source string `json:"source,omitempty"`
	// Note is the operator's reason for a curation (curated rows only).
	Note string `json:"note,omitempty"`
}

// ListModels returns an account's catalog with protocol verdicts.
func (d *DB) ListModels(ctx context.Context, accountID int64) ([]ModelEntryRow, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT model_id, COALESCE(display_name,''), COALESCE(owned_by,''),
       proto_openai, proto_anthropic, proto_responses, proto_tested_at, COALESCE(proto_pin,''),
       COALESCE(proto_source,''), COALESCE(proto_note,'')
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
		if err := rows.Scan(&m.ID, &m.Display, &m.OwnedBy, &o, &a, &r, &tested, &m.Pin, &m.Source, &m.Note); err != nil {
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
// A curated row is left alone: the operator's curation outranks a probe until
// released via SetModelCurated, so a sweep can never silently undo one.
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
INSERT INTO models (account_id, model_id, %s, proto_tested_at, proto_source)
VALUES (?,?,?,strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'),'probe')
ON CONFLICT(account_id, model_id) DO UPDATE SET
  %s=excluded.%s, proto_tested_at=excluded.proto_tested_at, proto_source='probe'
WHERE COALESCE(proto_source,'') <> 'curated'`,
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
INSERT INTO models (account_id, model_id, %s, proto_tested_at, proto_source)
VALUES (?,?,?,strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'),'probe')
ON CONFLICT(account_id, model_id) DO UPDATE SET
  %s=excluded.%s, proto_tested_at=excluded.proto_tested_at, proto_source='probe'
WHERE COALESCE(proto_source,'') <> 'curated'`, col, col, col))
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

// ProtoEdit is one surface's three-state edit on a curated row: Present=false
// keeps the recorded verdict, Value=nil clears it to untested, Value non-nil
// records the verdict.
type ProtoEdit struct {
	Present bool
	Value   *bool
}

// SetModelCurated records operator-curated protocol verdicts with provenance.
//
// release=false stamps proto_source='curated' with the note and clears
// proto_tested_at — nothing was probed, so no probe timestamp is fabricated.
// Until released, probe writes (SetModelProto/Protos) skip the row entirely.
// release=true hands the row back to the sweep: verdicts stay as recorded,
// source and note clear, and the next probe write applies again.
//
// ErrNotFound when the model row does not exist: curation never creates rows.
func (d *DB) SetModelCurated(ctx context.Context, accountID int64, modelID, note string, edits map[string]ProtoEdit, release bool) error {
	cols := map[string]string{"openai": "proto_openai", "anthropic": "proto_anthropic", "responses": "proto_responses"}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		var res sql.Result
		var err error
		if release {
			res, err = tx.ExecContext(ctx,
				`UPDATE models SET proto_source=NULL, proto_note=NULL
WHERE account_id=? AND model_id=?`, accountID, modelID)
		} else {
			set := []string{}
			args := []any{}
			for surface, edit := range edits {
				col, ok := cols[surface]
				if !ok {
					return fmt.Errorf("store: unknown surface %q", surface)
				}
				if !edit.Present {
					continue
				}
				set = append(set, col+"=?")
				args = append(args, boolPtrToInt(edit.Value))
			}
			set = append(set, "proto_source='curated'", "proto_note=?", "proto_tested_at=NULL")
			args = append(args, note, accountID, modelID)
			res, err = tx.ExecContext(ctx,
				`UPDATE models SET `+strings.Join(set, ", ")+` WHERE account_id=? AND model_id=?`, args...)
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

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
   balance, balance_unit, cost_total, cost_today, tokens_today, requests_today,
   resets_at, raw, ok, err)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			keyID, kind, q.PercentRolling, q.PercentWeekly, q.PercentMonthly,
			q.Balance, nullIfEmpty(q.BalanceUnit), q.CostTotal, q.CostToday,
			i64OrNil(q.TokensToday), i64OrNil(q.ReqsToday),
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
       q.balance, q.balance_unit, q.cost_total, q.cost_today,
       q.tokens_today, q.requests_today,
       q.resets_at,
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
		var tokToday, reqToday sql.NullInt64
		var unit, resets sql.NullString
		var ok bool
		if err := rows.Scan(&keyID, &acctID, &label, &hint, &kind, &pr, &pw, &pm,
			&bal, &unit, &ct, &cd, &tokToday, &reqToday, &resets, &readAt, &ok); err != nil {
			return nil, err
		}
		item := map[string]any{
			"key_id": keyID, "account_id": acctID, "label": label, "hint": hint,
			"kind": kind, "read_at": readAt, "ok": ok,
			"percent_rolling": nullFloat(pr), "percent_weekly": nullFloat(pw),
			"percent_monthly": nullFloat(pm), "balance": nullFloat(bal),
			"unit": nullStr(unit), "cost_total": nullFloat(ct),
			"cost_today":   nullFloat(cd),
			"tokens_today": nullInt(tokToday), "requests_today": nullInt(reqToday),
			"resets_at": nullStr(resets),
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
	// Weight is kept for API compatibility only: routing never used it, so it
	// is accepted on input and always stored and reported as 1.
	Weight  int   `json:"weight"`
	Enabled *bool `json:"enabled"`
	// HoldUntilFailure (block_round_robin only): the hop keeps the block while
	// it succeeds and releases it on a hop-attributable failure. Nullable so a
	// PATCH that omits it keeps the stored value.
	HoldUntilFailure *bool `json:"hold_until_failure,omitempty"`
}

// Combo is the admin-API combo shape.
type Combo struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Strategy    string `json:"strategy"`
	StickyIdleS int    `json:"sticky_idle_s"`
	// BlockSize is how many consecutive dispatches one hop serves before the
	// rotation advances (block_round_robin). 1 = plain per-request round robin.
	BlockSize            int        `json:"block_size"`
	Enabled              bool       `json:"enabled"`
	CompressionProfileID *int64     `json:"compression_profile_id"`
	ContextSize          int64      `json:"context_size"`
	Notes                string     `json:"notes,omitempty"`
	CreatedAt            string     `json:"created_at"`
	Hops                 []ComboHop `json:"hops"`
}

// ValidStrategies is the single source of truth for strategy names.
//
// This list IS the validation: the schema deliberately carries no CHECK on
// `strategy`, because SQLite cannot alter a constraint, so a CHECK would make
// every new strategy a full table rebuild (as block_round_robin was).
func ValidStrategies() []string {
	return []string{"failover", "true_round_robin", "sticky_last_good", "least_used", "block_round_robin"}
}

// ValidateCombo checks the payload before writing (422 on failure).
func ValidateCombo(c Combo) error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("name is required")
	}
	if c.ContextSize <= 0 || c.ContextSize > 10000000 {
		return errors.New("context_size must be between 1 and 10000000 tokens")
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
	// 0 is accepted as "unset" (the JSON zero value); UpsertCombo normalises it
	// to 1 before writing, which is also what the column CHECK allows.
	if c.BlockSize < 0 || c.BlockSize > 1000 {
		return errors.New("block_size must be between 1 and 1000")
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
SELECT id, name, strategy, sticky_idle_s, enabled, compression_profile_id, context_size,
       block_size, COALESCE(notes,''), created_at
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
			&cpid, &c.ContextSize, &c.BlockSize, &c.Notes, &c.CreatedAt); err != nil {
			return nil, err
		}
		if c.ContextSize == 0 {
			c.ContextSize = 200000
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
SELECT combo_id, account_id, model_id, weight, enabled, hold_until_failure
FROM combo_hops ORDER BY combo_id, position`)
	if err != nil {
		return nil, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var comboID int64
		var h ComboHop
		var hold int
		if err := hrows.Scan(&comboID, &h.AccountID, &h.ModelID, &h.Weight, &h.Enabled, &hold); err != nil {
			return nil, err
		}
		h.Weight = 1 // rows saved before weights were removed may hold more
		if idx, ok := byID[comboID]; ok {
			if hold != 0 {
				held := true
				h.HoldUntilFailure = &held
			}
			out[idx].Hops = append(out[idx].Hops, h)
		}
	}
	return out, hrows.Err()
}

// UpsertCombo creates or replaces a combo by name (idempotent POST per the
// API contract). Hops replace the whole ordered list; positions = array order.
func (d *DB) UpsertCombo(ctx context.Context, c Combo) (int64, error) {
	if c.ContextSize == 0 {
		c.ContextSize = 200000
	}
	if c.BlockSize == 0 {
		c.BlockSize = 1
	}
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
INSERT INTO combos (name, strategy, sticky_idle_s, enabled, compression_profile_id, context_size, block_size, notes)
VALUES (?,?,?,?,COALESCE(?, (SELECT id FROM compression_profiles WHERE 0)),?,?,?)
ON CONFLICT(name) DO UPDATE SET
  strategy=excluded.strategy, sticky_idle_s=excluded.sticky_idle_s,
  enabled=excluded.enabled, compression_profile_id=excluded.compression_profile_id,
  context_size=excluded.context_size, block_size=excluded.block_size, notes=excluded.notes`,
			c.Name, c.Strategy, c.StickyIdleS, boolToInt(c.Enabled), cpid, c.ContextSize, c.BlockSize, c.Notes)
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
		const weight = 1 // see ComboHop.Weight: accepted, never stored as given
		held := h.HoldUntilFailure != nil && *h.HoldUntilFailure
		if _, err := tx.ExecContext(ctx, `
INSERT INTO combo_hops (combo_id, position, account_id, model_id, weight, enabled, hold_until_failure)
VALUES (?,?,?,?,?,?,?)`, comboID, i, h.AccountID, h.ModelID, weight, boolToInt(enabled), boolToInt(held)); err != nil {
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
SELECT id, name, strategy, sticky_idle_s, enabled, compression_profile_id, context_size,
       block_size, COALESCE(notes,''), created_at
FROM combos WHERE name = ?`, name)
	var c Combo
	var cpid sql.NullInt64
	if err := row.Scan(&c.ID, &c.Name, &c.Strategy, &c.StickyIdleS, &c.Enabled,
		&cpid, &c.ContextSize, &c.BlockSize, &c.Notes, &c.CreatedAt); err != nil {
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
SELECT account_id, model_id, weight, enabled, hold_until_failure
FROM combo_hops WHERE combo_id = ? ORDER BY position`, c.ID)
	if err != nil {
		return nil, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var h ComboHop
		var hold int
		if err := hrows.Scan(&h.AccountID, &h.ModelID, &h.Weight, &h.Enabled, &hold); err != nil {
			return nil, err
		}
		h.Weight = 1 // rows saved before weights were removed may hold more
		if hold != 0 {
			held := true
			h.HoldUntilFailure = &held
		}
		c.Hops = append(c.Hops, h)
	}
	return &c, hrows.Err()
}

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
	AllowedModels string   `json:"allowed_models"`
	ExpiresAt     string   `json:"expires_at"`
	LastUsedAt    *string  `json:"last_used_at"`
	CreatedAt     string   `json:"created_at"`
}

// ListTokens returns all client tokens (hints only).
func (d *DB) ListTokens(ctx context.Context) ([]TokenSummary, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT id, name, token_hash, roles, enabled, cap_window, cap_tokens,
       allowed_models, expires_at,
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
			&t.CapWindow, &t.CapTokens, &t.AllowedModels, &t.ExpiresAt, &lastUsed, &t.CreatedAt); err != nil {
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
	Name          string   `json:"name"`
	Roles         []string `json:"roles"`
	Token         string   `json:"token"`
	AllowedModels string   `json:"allowed_models"`
	ExpiresAt     string   `json:"expires_at"`
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
	if _, err := normalizeExpiry(in.ExpiresAt); err != nil {
		return err
	}
	return nil
}

// normalizeExpiry stores RFC3339 UTC. A bare YYYY-MM-DD means end of that day
// (UTC) so a date picker does not expire a key at midnight at the start.
func normalizeExpiry(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
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
	roles := in.Roles
	if len(roles) == 0 {
		roles = []string{"infer"}
	}
	expires, err := normalizeExpiry(in.ExpiresAt)
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
			exp, err := normalizeExpiry(*expiresAt)
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

// ExportLedgerRows returns a LIVE row cursor over the whole call ledger,
// newest first. The caller MUST Close it.
//
// This exists so the HTTP handler can stream: the old shape built one
// []map[string]any of the entire ledger and encoded it in a single write, so
// memory scaled with ledger size — and a disaster-recovery export that OOMs
// the box is not a recovery file. A row cursor keeps one row live at a time,
// which makes the export's footprint flat and the file COMPLETE: the 10k cap
// that used to bound it is gone, because bounding a recovery file means
// silently omitting history from the thing you recover WITH.
//
// The row projection lives in scanExportRow so this and ExportAll cannot drift.
func (d *DB) ExportLedgerRows(ctx context.Context) (*sql.Rows, error) {
	return d.r.QueryContext(ctx, `
SELECT ts, client, surface, alias, account, model, status, stream,
       ttft_ms, total_ms, tokens_in, tokens_out, tokens_cached_read,
       tokens_cached_write, reasoning_tokens, status
FROM calls ORDER BY id DESC`)
}

// ScanExportRow projects one ledger row into the export's JSON shape. It is
// exported alongside the cursor so the streaming path and ExportAll share ONE
// projection — a second copy is how a DR file quietly loses a column.
func ScanExportRow(rows *sql.Rows) (map[string]any, error) {
	var ts, client, surface, alias, account, model string
	var status, stream int
	var ttft, total, tin, tout, cread, cwrite, reasoning sql.NullInt64
	var statusDup int
	if err := rows.Scan(&ts, &client, &surface, &alias, &account, &model, &status,
		&stream, &ttft, &total, &tin, &tout, &cread, &cwrite, &reasoning, &statusDup); err != nil {
		return nil, err
	}
	return map[string]any{
		"ts": ts, "client": client, "surface": surface, "alias": alias,
		"account": account, "model": model, "status": status, "stream": stream == 1,
		"ttft_ms": nullInt(ttft), "total_ms": nullInt(total),
		"tokens_in": nullInt(tin), "tokens_out": nullInt(tout),
		"cached_read": nullInt(cread), "cached_write": nullInt(cwrite),
		"reasoning": nullInt(reasoning),
	}, nil
}

// ExportConfig builds the dump WITHOUT the ledger: the small, bounded half.
// Kept separate from the ledger so a caller can validate (and a handler can
// answer) before committing to a long stream.
func (d *DB) ExportConfig(ctx context.Context) (*Export, error) {
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
	return &Export{
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
		SchemaVersion: d.SchemaVersion(),
		Accounts:      accounts,
		Combos:        combos,
		Tokens:        tokens,
		Compression:   profiles,
		Quotas:        quotas,
	}, nil
}

// ExportAll builds the whole dump in memory. includeLedger adds the call
// history, which is why it is opt-in.
//
// Prefer ExportConfig + ExportLedgerRows for anything HTTP-facing: this
// materialises every row. It stays for non-HTTP callers (cmd, tooling, tests)
// where holding the result is fine and a single value is simpler.
func (d *DB) ExportAll(ctx context.Context, includeLedger bool) (*Export, error) {
	exp, err := d.ExportConfig(ctx)
	if err != nil {
		return nil, err
	}
	if !includeLedger {
		return exp, nil
	}
	rows, err := d.ExportLedgerRows(ctx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	exp.Ledger = []map[string]any{}
	for rows.Next() {
		m, err := ScanExportRow(rows)
		if err != nil {
			return nil, err
		}
		exp.Ledger = append(exp.Ledger, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
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

// i64OrNil passes a *int64 straight through: nil inserts NULL (no data), and
// a real 0 survives as 0. A helper that collapses 0→nil would corrupt a
// legitimate "zero requests today" read.
func i64OrNil(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
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
