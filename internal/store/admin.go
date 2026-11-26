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
	RateLimit        int64  `json:"rate_limit"`
	RateWindow       string `json:"rate_window"`
	ProbeDelayMS     int    `json:"probe_delay_ms"`
	Notes            string `json:"notes,omitempty"`
	ManagementKeySet bool   `json:"management_key_set"`
	// ManagementKeyExpiresAt: when the stored web-session cookie (Command
	// Code) expires, RFC3339, as read from the pasted session_data. Empty
	// when unknown — never guessed.
	ManagementKeyExpiresAt string `json:"management_key_expires_at,omitempty"`
	// CatalogSyncedAt is when a catalog listing was last saved (refresh, key
	// add, manual sync); empty = never.
	CatalogSyncedAt string         `json:"catalog_synced_at,omitempty"`
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
	TokensToday    *int64   `json:"tokens_today"`
	RequestsToday  *int64   `json:"requests_today"`
	ResetsAt       *string  `json:"resets_at"`
	ReadAt         string   `json:"read_at"`
	// Groups is set when the provider limits its models in named groups.
	Groups []provider.QuotaGroup `json:"groups,omitempty"`
}

// ListAccounts returns every account with keys, latest quota and protocol
// counts — one query set so the providers page needs no N+1 fan-out.
func (d *DB) ListAccounts(ctx context.Context) ([]AccountSummary, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT id, name, namespace, kind, base_url, enabled, requires_session_header,
       quota_mode, cap_window, cap_tokens, COALESCE(probe_delay_ms,400),
       COALESCE(rate_limit,0), COALESCE(rate_window,''),
       COALESCE(notes,''), created_at, updated_at, COALESCE(usage_convention,'auto'),
       COALESCE(management_key_plain,''), COALESCE(catalog_synced_at,'')
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
			&a.Notes, &a.CreatedAt, &a.UpdatedAt, &a.UsageConvention, &managementKey, &a.CatalogSyncedAt); err != nil {
			return nil, err
		}
		a.ManagementKeySet = strings.TrimSpace(managementKey) != ""
		if a.ManagementKeySet && d.crypto != nil && strings.HasPrefix(managementKey, cryptoPrefix) {
			if dec, err := d.crypto.Decrypt(managementKey); err == nil {
				managementKey = dec
			}
		}
		if t, ok := provider.CommandCodeCookieExpiry(managementKey); ok {
			a.ManagementKeyExpiresAt = t.UTC().Format(time.RFC3339)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := d.hydrateAccounts(ctx, out); err != nil {
		return nil, err
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

// hydrateAccounts fills keys, the latest quota and protocol counts for every
// account in three queries, however many accounts there are. (It was three
// queries per account: the Providers page and the overview paid 3N+1.)
func (d *DB) hydrateAccounts(ctx context.Context, accts []AccountSummary) error {
	byID := make(map[int64]*AccountSummary, len(accts))
	for i := range accts {
		a := &accts[i]
		a.Keys = []KeySummary{} // API contract: collections are arrays, including when empty.
		a.ProtocolSupport = map[string]int{"chat": 0, "anthropic": 0, "responses": 0, "untested": 0}
		byID[a.ID] = a
	}
	if len(accts) == 0 {
		return nil
	}

	krows, err := d.r.QueryContext(ctx, `
SELECT account_id, id, label, key_hint, enabled, last_test_at, last_test_ok,
       COALESCE(last_test_detail,''), added_at
FROM provider_keys ORDER BY account_id, id`)
	if err != nil {
		return err
	}
	defer krows.Close()
	for krows.Next() {
		var acct int64
		var k KeySummary
		var lastTest sql.NullString
		var ok sql.NullBool
		if err := krows.Scan(&acct, &k.ID, &k.Label, &k.Hint, &k.Enabled, &lastTest, &ok,
			&k.LastTestDetail, &k.AddedAt); err != nil {
			return err
		}
		a := byID[acct]
		if a == nil {
			continue
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
	krows.Close()

	// The latest quota read across each account's keys (most recent wins).
	qrows, err := d.r.QueryContext(ctx, `
SELECT account_id, kind, percent_rolling, percent_weekly, percent_monthly,
       balance, balance_unit, cost_total, cost_today, tokens_today, requests_today,
       resets_at, read_at, raw
FROM (
  SELECT k.account_id, q.kind, q.percent_rolling, q.percent_weekly, q.percent_monthly,
         q.balance, q.balance_unit, q.cost_total, q.cost_today,
         q.tokens_today, q.requests_today, q.resets_at, q.read_at, COALESCE(q.raw,'') AS raw,
         ROW_NUMBER() OVER (PARTITION BY k.account_id ORDER BY q.read_at DESC, q.id DESC) AS rn
  FROM quota_snapshots q
  JOIN provider_keys k ON k.id = q.provider_key_id
) WHERE rn = 1`)
	if err != nil {
		return err
	}
	defer qrows.Close()
	for qrows.Next() {
		var acct int64
		var q QuotaSummary
		var qReadAt, qRaw string
		var tokToday, reqToday sql.NullInt64
		if err := qrows.Scan(&acct, &q.Kind, &q.PercentRolling, &q.PercentWeekly, &q.PercentMonthly,
			&q.Balance, &q.Unit, &q.CostTotal, &q.CostToday, &tokToday, &reqToday,
			&q.ResetsAt, &qReadAt, &qRaw); err != nil {
			return err
		}
		a := byID[acct]
		if a == nil {
			continue
		}
		q.ReadAt = qReadAt
		q.Groups = provider.QuotaGroups(qRaw)
		if tokToday.Valid {
			v := tokToday.Int64
			q.TokensToday = &v
		}
		if reqToday.Valid {
			v := reqToday.Int64
			q.RequestsToday = &v
		}
		a.Quota = &q
	}
	if err := qrows.Err(); err != nil {
		return err
	}
	qrows.Close()

	mrows, err := d.r.QueryContext(ctx, `
SELECT account_id, COUNT(*),
       COALESCE(SUM(CASE WHEN proto_chat=1 THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN proto_anthropic=1 THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN proto_responses=1 THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN proto_chat IS NULL AND proto_anthropic IS NULL
                          AND proto_responses IS NULL THEN 1 ELSE 0 END),0)
FROM models GROUP BY account_id`)
	if err != nil {
		return err
	}
	defer mrows.Close()
	for mrows.Next() {
		var acct int64
		var count, chat, anthropic, responses, untested int
		if err := mrows.Scan(&acct, &count, &chat, &anthropic, &responses, &untested); err != nil {
			return err
		}
		a := byID[acct]
		if a == nil {
			continue
		}
		a.ModelsCount = count
		a.ProtocolSupport["chat"] = chat
		a.ProtocolSupport["anthropic"] = anthropic
		a.ProtocolSupport["responses"] = responses
		a.ProtocolSupport["untested"] = untested
	}
	return mrows.Err()
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
	// required verbatim when present. copilot-bridge is NOT a sentinel kind:
	// it dials the loopback bridge over http, so the ordinary base-URL rule
	// applies to it.
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
	// nil = left out of the PATCH: keep. An empty map or an explicit 0 clears.
	// These were not carried over, so turning an account off in the dashboard
	// wiped its custom headers, token cap and context size.
	if in.CustomHeaders == nil {
		in.CustomHeaders = cur.CustomHeaders
	}
	if in.CapTokens == nil {
		v := cur.CapTokens
		in.CapTokens = &v
	}
	if in.MaxContextTokens == nil {
		v := cur.MaxContextTokens
		in.MaxContextTokens = &v
	}
	// The same rules as creating an account: a PATCH must not write a
	// namespace with '/' or a sentinel kind's base URL that the create path
	// refuses.
	if err := ValidateAccountInput(in); err != nil {
		return err
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
	// An enable/disable flip changes which accounts the auto_free pools draw on.
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error { return d.syncAutoFree(ctx, tx, 0) })
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
			// A new account never takes an id an old one used: SQLite reuses
			// the highest rowid after a delete, and the ledger, usage counters
			// and rate history still key on it, so the new account would
			// inherit the deleted one's usage.
			var next int64
			if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(m), 0) + 1 FROM (
  SELECT MAX(id) AS m FROM accounts
  UNION ALL SELECT MAX(account_id) FROM calls
  UNION ALL SELECT MAX(account_id) FROM usage_counters)`).Scan(&next); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `
INSERT INTO accounts (id, name, namespace, kind, base_url, enabled, requires_session_header,
                      quota_mode, custom_headers, probe_delay_ms, cap_window, cap_tokens,
                      rate_limit, rate_window, notes, usage_convention, max_context_tokens,
                      management_key_plain)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				next, in.Name, in.Namespace, string(kind), in.BaseURL, enabled, requires,
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
		// Usage counters key on the id and have no foreign key: without this
		// they outlive the account.
		if _, err := tx.ExecContext(ctx, `DELETE FROM usage_counters WHERE account_id=?`, id); err != nil {
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

// isUniqueViolation matches SQLite's UNIQUE constraint text without
// depending on driver-specific error types.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Only UNIQUE: a FOREIGN KEY, CHECK or NOT NULL failure is not "name in
	// use", and reporting it as 409 hid the real error.
	return strings.Contains(msg, "UNIQUE constraint failed")
}
