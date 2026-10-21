package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// An auto_free combo has no hand-written hop list: its hops are the free
// models in the account catalogs, rebuilt whenever a catalog is replaced or
// the combo is saved. "Free" is what vendors can actually be seen to say
// about a model — the id carries a `:free` (OpenRouter, Command Code) or
// `-free` (Opencode, Vercel AI Gateway) suffix. Account-level free tiers
// (Groq, Google AI Studio, ...) are not model-level facts, so they are out of
// scope here, and so are promotions with no id marker (Ollama Cloud's
// free-for-now `beam`): list those in auto_free_include and they join the pool
// whenever the catalog still carries them. One deliberate exception: Cloudflare
// Workers AI's free-tier models carry no marker either, so a built-in list
// (provider.IsCloudflareFreeChatModel) names them — chat-capable ones only,
// and only on a host-verified Workers AI account.

// IsFreeModelID reports whether a catalog model id is marked free by the vendor.
func IsFreeModelID(id string) bool {
	id = strings.ToLower(id)
	return strings.HasSuffix(id, ":free") || strings.HasSuffix(id, "-free")
}

// FreeModelIDs lists the models in one account's catalog that auto_free
// pools treat as free: an id marked free by the vendor, or a built-in free
// list (Cloudflare Workers AI). The same rule syncAutoFree applies, so what
// gets probed is exactly what can join a free combo.
func (d *DB) FreeModelIDs(ctx context.Context, accountID int64) ([]string, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT m.model_id, a.kind, a.base_url
FROM models m JOIN accounts a ON a.id = m.account_id
WHERE m.account_id = ?
ORDER BY m.model_id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, kind, baseURL string
		if err := rows.Scan(&id, &kind, &baseURL); err != nil {
			return nil, err
		}
		if IsFreeModelID(id) || provider.IsCloudflareFreeChatModel(kind, baseURL, id) {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// SyncAutoFree rebuilds every auto_free combo. Catalog and combo writes
// already do this in their own transaction; this is for writes that change
// eligibility from outside them — a probe verdict that proves a free model
// broken on every surface drops it from the pools now, not at the next
// catalog refresh.
func (d *DB) SyncAutoFree(ctx context.Context) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error { return d.syncAutoFree(ctx, tx, 0) })
}

// alterComboAutoFree adds combos.auto_free / auto_free_accounts on databases
// created before v25. Guarded by column checks: ALTER TABLE ADD COLUMN is not
// idempotent. Existing combos stay hand-managed (auto_free = 0).
func (d *DB) alterComboAutoFree(ctx context.Context) error {
	exists, err := hasTable(ctx, d.w, "combos")
	if err != nil || !exists {
		return err
	}
	for col, ddl := range map[string]string{
		"auto_free":          `ALTER TABLE combos ADD COLUMN auto_free INTEGER NOT NULL DEFAULT 0`,
		"auto_free_accounts": `ALTER TABLE combos ADD COLUMN auto_free_accounts TEXT NOT NULL DEFAULT ''`,
		"auto_free_include":  `ALTER TABLE combos ADD COLUMN auto_free_include TEXT NOT NULL DEFAULT ''`,
	} {
		if ok, err := hasColumn(ctx, d.w, "combos", col); err != nil {
			return err
		} else if ok {
			continue
		}
		if _, err := d.w.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	return nil
}

func formatAccountIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// FreeInclude names one model that is in an auto_free pool regardless of its id.
type FreeInclude struct {
	AccountID int64  `json:"account_id"`
	ModelID   string `json:"model_id"`
}

func formatFreeIncludes(in []FreeInclude) string {
	if len(in) == 0 {
		return ""
	}
	b, _ := json.Marshal(in)
	return string(b)
}

func parseFreeIncludes(s string) []FreeInclude {
	out := []FreeInclude{}
	if s != "" {
		_ = json.Unmarshal([]byte(s), &out)
	}
	return out
}

func parseAccountIDs(s string) []int64 {
	out := []int64{}
	for _, p := range strings.Split(s, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}

// syncAutoFree rebuilds the hops of every auto_free combo (or only comboID
// when non-zero). A hop's `enabled` / hold flags survive a rebuild so an
// operator can mute one free model without the next sync resurrecting it.
// Models whose protocol probe failed on every surface are left out. The combo
// is untouched when the derived list equals the stored one.
func (d *DB) syncAutoFree(ctx context.Context, tx *sql.Tx, comboID int64) error {
	q := `SELECT id, auto_free_accounts, auto_free_include FROM combos WHERE auto_free = 1`
	args := []any{}
	if comboID != 0 {
		q += ` AND id = ?`
		args = append(args, comboID)
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	type target struct {
		id       int64
		accounts []int64
		include  map[string]bool
	}
	var targets []target
	for rows.Next() {
		var t target
		var accts, incl string
		if err := rows.Scan(&t.id, &accts, &incl); err != nil {
			rows.Close()
			return err
		}
		t.accounts = parseAccountIDs(accts)
		t.include = map[string]bool{}
		for _, in := range parseFreeIncludes(incl) {
			t.include[fmt.Sprintf("%d/%s", in.AccountID, in.ModelID)] = true
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}

	crows, err := tx.QueryContext(ctx, `
SELECT m.account_id, m.model_id, a.kind, a.base_url
FROM models m JOIN accounts a ON a.id = m.account_id
WHERE a.enabled = 1
  AND NOT (m.proto_tested_at IS NOT NULL AND COALESCE(m.proto_openai,0) = 0
           AND COALESCE(m.proto_anthropic,0) = 0 AND COALESCE(m.proto_responses,0) = 0)
ORDER BY m.account_id, m.model_id`)
	if err != nil {
		return err
	}
	var cands []ComboHop
	builtinFree := map[string]bool{} // "account/model" free by a built-in list, not by id suffix
	for crows.Next() {
		var h ComboHop
		var kind, baseURL string
		if err := crows.Scan(&h.AccountID, &h.ModelID, &kind, &baseURL); err != nil {
			crows.Close()
			return err
		}
		if provider.IsCloudflareFreeChatModel(kind, baseURL, h.ModelID) {
			builtinFree[fmt.Sprintf("%d/%s", h.AccountID, h.ModelID)] = true
		}
		cands = append(cands, h)
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return err
	}

	for _, t := range targets {
		prev := map[string]ComboHop{}
		var prevOrder []string
		hrows, err := tx.QueryContext(ctx, `
SELECT account_id, model_id, enabled, hold_until_failure FROM combo_hops
WHERE combo_id = ? ORDER BY position`, t.id)
		if err != nil {
			return err
		}
		for hrows.Next() {
			var h ComboHop
			var en, hold int
			if err := hrows.Scan(&h.AccountID, &h.ModelID, &en, &hold); err != nil {
				hrows.Close()
				return err
			}
			e, ho := en != 0, hold != 0
			h.Enabled, h.HoldUntilFailure = &e, &ho
			k := fmt.Sprintf("%d/%s", h.AccountID, h.ModelID)
			prev[k] = h
			prevOrder = append(prevOrder, k)
		}
		hrows.Close()
		if err := hrows.Err(); err != nil {
			return err
		}

		allowed := map[int64]bool{}
		for _, id := range t.accounts {
			allowed[id] = true
		}
		var next []ComboHop
		var nextOrder []string
		for _, h := range cands {
			k := fmt.Sprintf("%d/%s", h.AccountID, h.ModelID)
			if !t.include[k] && (!(IsFreeModelID(h.ModelID) || builtinFree[k]) || (len(allowed) > 0 && !allowed[h.AccountID])) {
				continue
			}
			if p, ok := prev[k]; ok {
				h.Enabled, h.HoldUntilFailure = p.Enabled, p.HoldUntilFailure
			}
			next = append(next, h)
			nextOrder = append(nextOrder, k)
		}
		if strings.Join(prevOrder, "\n") == strings.Join(nextOrder, "\n") {
			continue
		}
		if err := d.replaceHops(ctx, tx, t.id, next); err != nil {
			return err
		}
	}
	return nil
}

// DefaultFreeComboName is the combo a fresh database is seeded with.
const DefaultFreeComboName = "ezllm-auto-free"

// seedFreeCombo gives a brand-new database an empty auto_free combo, so a
// first-time user has a working `ezllm-auto-free` model name the moment the first
// provider's catalog lands. It runs only in the fresh-database branch of
// migrate: deleting the combo later is a decision that sticks. Round robin,
// because free tiers are rate-limited per key and failover would pin every
// request on the first hop until it 429s.
func (d *DB) seedFreeCombo(ctx context.Context) error {
	_, err := d.w.ExecContext(ctx, `
INSERT OR IGNORE INTO combos (name, strategy, enabled, auto_free, notes)
VALUES (?, 'true_round_robin', 1, 1, 'Auto-managed: every free-marked model from your providers.')`,
		DefaultFreeComboName)
	return err
}
