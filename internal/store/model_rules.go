package store

// M5 rules engine — model_rules CRUD.
//
// One row per (account, model), mirroring models' UNIQUE(account_id, model_id),
// so the router's Eligible seam (accountID, modelID) looks a rule up with
// exactly the key it already has.
//
// Validation lives here, next to the table it guards, rather than in the
// server: the same constraint must hold for config seeding and for the admin
// API, and one validator covers both (it is also why cap_window accepts only
// what ValidateAccount accepts).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrRuleNotFound is returned when a rule does not exist (distinguishes "no
// rule" from a database failure — callers must not swallow the latter).
var ErrRuleNotFound = errors.New("model rule not found")

// ModelRule is the admin-API rule shape. A nil window (Start/End empty) means
// the model is allowed at all times; CapTokens 0 means no usage cap.
type ModelRule struct {
	ID        int64  `json:"id"`
	AccountID int64  `json:"account_id"`
	ModelID   string `json:"model_id"`
	CapTokens int64  `json:"cap_tokens"`
	CapWindow string `json:"cap_window"`
	WinStart  string `json:"win_start,omitempty"` // 'HH:MM' local to WinTZ
	WinEnd    string `json:"win_end,omitempty"`   // end<start wraps midnight
	WinDays   string `json:"win_days,omitempty"`  // csv 0-6 (Sun=0); "" = every day
	WinTZ     string `json:"win_tz"`
	Enabled   bool   `json:"enabled"`
	Note      string `json:"note,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

var (
	// hhmmRE is deliberately strict: an unvalidated 'HH:MM' becomes an
	// unparseable window the router would have to reject at request time.
	hhmmRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	// tzRE accepts IANA names ('Asia/Kuala_Lumpur') and fixed offsets
	// ('+08:00'), both of which time.LoadLocation understands.
	tzRE = regexp.MustCompile(`^(Asia/|America/|Europe/|Africa/|Australia/|Pacific/|Atlantic/|Indian/|US/|UTC|[+-][0-9]{2}:[0-9]{2})`)
)

// ValidateModelRule checks the invariant set once, so both the admin API and
// config seeding fail identically. Returns a plain error suitable for a 400.
func ValidateModelRule(r *ModelRule) error {
	if strings.TrimSpace(r.ModelID) == "" {
		return errors.New("model_id is required")
	}
	if r.CapTokens < 0 {
		return errors.New("cap_tokens must be >= 0 (0 = unlimited)")
	}
	// Same enum as ValidateAccount (admin.go) — one accepted set, not two.
	switch r.CapWindow {
	case "", "5h", "daily", "weekly", "monthly":
		if r.CapWindow == "" {
			r.CapWindow = "monthly"
		}
	default:
		return errors.New("cap_window must be 5h|daily|weekly|monthly")
	}

	start, end := strings.TrimSpace(r.WinStart), strings.TrimSpace(r.WinEnd)
	// A window is either fully set or fully unset — half a window would
	// silently become "always allowed" if we let the empty side pass.
	if start == "" && end == "" {
		r.WinStart, r.WinEnd = "", ""
	} else {
		if !hhmmRE.MatchString(start) {
			return fmt.Errorf("win_start %q must be HH:MM (00:00-23:59)", start)
		}
		if !hhmmRE.MatchString(end) {
			return fmt.Errorf("win_end %q must be HH:MM (00:00-23:59)", end)
		}
		r.WinStart, r.WinEnd = start, end
		if start == end {
			return errors.New("win_start and win_end must differ (an identical pair is an empty window, not all-day)")
		}
	}

	days := strings.TrimSpace(r.WinDays)
	if days != "" {
		seen := map[int]bool{}
		for _, part := range strings.Split(days, ",") {
			part = strings.TrimSpace(part)
			var n int
			if _, err := fmt.Sscanf(part, "%d", &n); err != nil || fmt.Sprintf("%d", n) != part {
				return fmt.Errorf("win_days %q: entries must be integers 0-6 (Sun=0)", part)
			}
			if n < 0 || n > 6 {
				return fmt.Errorf("win_days entry %d out of range 0-6 (Sun=0)", n)
			}
			if seen[n] {
				return fmt.Errorf("win_days entry %d is duplicated", n)
			}
			seen[n] = true
		}
	}
	r.WinDays = days

	if tz := strings.TrimSpace(r.WinTZ); tz != "" {
		if !tzRE.MatchString(tz) {
			return fmt.Errorf("win_tz %q must be an IANA zone (Asia/Kuala_Lumpur) or offset (+08:00)", tz)
		}
		r.WinTZ = tz
	} else {
		r.WinTZ = "Asia/Kuala_Lumpur"
	}
	return nil
}

// ListModelRules returns every rule, newest first, optionally for one account.
func (d *DB) ListModelRules(ctx context.Context, accountID int64) ([]ModelRule, error) {
	q := `SELECT id, account_id, model_id, cap_tokens, cap_window,
                COALESCE(win_start,''), COALESCE(win_end,''), COALESCE(win_days,''),
                win_tz, enabled, note, updated_at
         FROM model_rules`
	args := []any{}
	if accountID != 0 {
		q += ` WHERE account_id = ?`
		args = append(args, accountID)
	}
	q += ` ORDER BY id DESC`
	rows, err := d.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list model rules: %w", err)
	}
	defer rows.Close()
	var out []ModelRule
	for rows.Next() {
		r, err := scanModelRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// GetModelRule fetches one rule by primary key.
func (d *DB) GetModelRule(ctx context.Context, id int64) (*ModelRule, error) {
	row := d.r.QueryRowContext(ctx, `SELECT id, account_id, model_id, cap_tokens, cap_window,
                COALESCE(win_start,''), COALESCE(win_end,''), COALESCE(win_days,''),
                win_tz, enabled, note, updated_at
         FROM model_rules WHERE id = ?`, id)
	r, err := scanModelRuleRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRuleNotFound
	}
	return r, err
}

// UpsertModelRule inserts or updates the single rule for (account, model).
// Returns the row id so the caller can round-trip the saved record.
func (d *DB) UpsertModelRule(ctx context.Context, in ModelRule) (int64, error) {
	if err := ValidateModelRule(&in); err != nil {
		return 0, err
	}
	var start, end, days any
	if in.WinStart != "" {
		start = in.WinStart
	}
	if in.WinEnd != "" {
		end = in.WinEnd
	}
	if in.WinDays != "" {
		days = in.WinDays
	}
	res, err := d.w.ExecContext(ctx, `
INSERT INTO model_rules (account_id, model_id, cap_tokens, cap_window,
                         win_start, win_end, win_days, win_tz, enabled, note, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
ON CONFLICT(account_id, model_id) DO UPDATE SET
  cap_tokens=excluded.cap_tokens, cap_window=excluded.cap_window,
  win_start=excluded.win_start, win_end=excluded.win_end,
  win_days=excluded.win_days, win_tz=excluded.win_tz,
  enabled=excluded.enabled, note=excluded.note,
  updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		in.AccountID, in.ModelID, in.CapTokens, in.CapWindow,
		start, end, days, in.WinTZ, boolInt(in.Enabled), in.Note)
	if err != nil {
		return 0, fmt.Errorf("upsert model rule: %w", err)
	}
	// The conflict path does not report the pre-existing row id, so resolve it.
	if _, err := res.RowsAffected(); err != nil {
		return 0, err
	}
	var id int64
	if err := d.w.QueryRowContext(ctx,
		`SELECT id FROM model_rules WHERE account_id = ? AND model_id = ?`,
		in.AccountID, in.ModelID).Scan(&id); err != nil {
		return 0, fmt.Errorf("resolve model rule id: %w", err)
	}
	return id, nil
}

// DeleteModelRule removes a rule. Deleting a rule means "no longer restricted",
// which is the natural way to lift a cap or a window.
func (d *DB) DeleteModelRule(ctx context.Context, id int64) error {
	res, err := d.w.ExecContext(ctx, `DELETE FROM model_rules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete model rule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrRuleNotFound
	}
	return nil
}

// ruleScanner is satisfied by *sql.Row and *sql.Rows.
type ruleScanner interface{ Scan(dest ...any) error }

func scanModelRuleRow(row ruleScanner) (*ModelRule, error) {
	var r ModelRule
	var enabled int
	err := row.Scan(&r.ID, &r.AccountID, &r.ModelID, &r.CapTokens, &r.CapWindow,
		&r.WinStart, &r.WinEnd, &r.WinDays, &r.WinTZ, &enabled, &r.Note, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("scan model rule: %w", err)
	}
	r.Enabled = enabled != 0
	return &r, nil
}

func scanModelRule(rows *sql.Rows) (*ModelRule, error) { return scanModelRuleRow(rows) }
