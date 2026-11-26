package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// ── export ──────────────────────────────────────────────────────────────────

// Export is the disaster-recovery dump: everything needed to re-seed a fresh
// instance EXCEPT credentials (hints only — key_plain is deliberately absent).
type ExportModel struct {
	AccountID int64 `json:"account_id"`
	ModelEntryRow
}

type ExportCompletion struct {
	Complete        bool   `json:"complete"`
	LedgerRequested bool   `json:"ledger_requested"`
	LedgerRows      int    `json:"ledger_rows"`
	Error           string `json:"error,omitempty"`
}

type Export struct {
	Completion             ExportCompletion           `json:"completion"`
	FormatVersion          int                        `json:"format_version"`
	Kind                   string                     `json:"kind"`
	Settings               map[string]json.RawMessage `json:"settings"`
	DatabaseBackupRequired bool                       `json:"database_backup_required"`
	Models                 []ExportModel              `json:"models"`
	Rules                  []ModelRule                `json:"model_rules"`
	Prices                 []Price                    `json:"model_prices"`
	Bindings               []ServiceBinding           `json:"service_bindings"`
	ExportedAt             string                     `json:"exported_at"`
	SchemaVersion          int                        `json:"schema_version"`
	Accounts               []AccountSummary           `json:"accounts"`
	Combos                 []Combo                    `json:"combos"`
	Tokens                 []TokenSummary             `json:"tokens"`
	Compression            []CompressionProfile       `json:"compression_profiles"`
	Quotas                 []map[string]any           `json:"latest_quotas"`
	Ledger                 []map[string]any           `json:"ledger,omitempty"`
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
	savedSettings, err := d.LoadSettings(ctx)
	if err != nil {
		return nil, err
	}
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
	rules, err := d.ListModelRules(ctx, 0)
	if err != nil {
		return nil, err
	}
	prices, err := d.ListPrices(ctx)
	if err != nil {
		return nil, err
	}
	bindings, err := d.ListServiceBindings(ctx)
	if err != nil {
		return nil, err
	}
	models := []ExportModel{}
	for _, account := range accounts {
		entries, err := d.ListModels(ctx, account.ID)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			models = append(models, ExportModel{AccountID: account.ID, ModelEntryRow: entry})
		}
	}
	return &Export{
		Settings: savedSettings.Values, FormatVersion: 1, Kind: "configuration_export", DatabaseBackupRequired: true,
		Completion: ExportCompletion{Complete: true},
		Models:     models, Rules: rules, Prices: prices, Bindings: bindings,
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
	exp.Completion.LedgerRequested = true
	exp.Ledger = []map[string]any{}
	for rows.Next() {
		m, err := ScanExportRow(rows)
		if err != nil {
			return nil, err
		}
		exp.Ledger = append(exp.Ledger, m)
		exp.Completion.LedgerRows++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return exp, nil
}
