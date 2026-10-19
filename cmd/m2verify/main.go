// Command m2verify runs the ledger's acceptance checks straight against a
// live database (read-only), so the results are real query output rather than
// assertion prose. Exit 1 = at least one invariant failed.
//
//	go run ./cmd/m2verify [path/to/ezllm.sqlite]
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// recentRows is how many of the newest ledger rows the usage check replays:
// it audits what the RUNNING code writes, not history the backfill owns.
const recentRows = 1000

func main() {
	path := "data/ezllm.sqlite"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer db.Close()

	fail := 0
	check := func(name string, r result) {
		fmt.Printf("\n══ %s ══\n", name)
		for _, l := range r.lines {
			fmt.Println("  " + l)
		}
		switch {
		case r.err != nil:
			fmt.Println("  ERROR:", r.err)
			fail++
		case r.ok:
			fmt.Println("  PASS")
		default:
			fmt.Println("  FAIL")
			fail++
		}
	}
	check("every input token lands in exactly one billed bucket (newest rows replayed from raw_usage)", usageAgreesWithRaw(db, recentRows))
	check("provider credentials are encrypted at rest", credentialsEncrypted(db))
	check("client tokens are stored hashed (sha256), never plaintext", clientTokensHashed(db))
	check("raw usage retained for audit", rawUsageRetained(db))
	check("streaming is incremental (ttft < total on the newest streams)", streamingIncremental(db))

	fmt.Printf("\n════════ RESULT: %d invariant failure(s) ════════\n", fail)
	if fail > 0 {
		os.Exit(1)
	}
}

type result struct {
	ok    bool
	lines []string
	err   error
}

// usageAgreesWithRaw replays each recent row's raw_usage through the current
// normalizer and compares the four billed counters. A disagreement is a token
// counted in the wrong bucket (or twice). Rows whose raw lacks the total
// field cannot be replayed and are counted, not judged.
func usageAgreesWithRaw(db *sql.DB, limit int) result {
	rows, err := db.Query(`SELECT c.id, c.surface, c.translated, c.raw_usage,
		c.tokens_in, c.tokens_out, c.tokens_cached_read, c.tokens_cached_write,
		COALESCE(a.usage_convention, 'auto')
		FROM calls c LEFT JOIN accounts a ON a.id = c.account_id
		WHERE c.raw_usage IS NOT NULL AND c.raw_usage <> ''
		  AND c.surface IN ('openai','anthropic','responses')
		ORDER BY c.id DESC LIMIT ?`, limit)
	if err != nil {
		return result{err: err}
	}
	defer rows.Close()
	var checked, unreplayable int
	var bad []string
	for rows.Next() {
		var id, translated, in, out, cr, cw int64
		var surface, raw, conv string
		if err := rows.Scan(&id, &surface, &translated, &raw, &in, &out, &cr, &cw, &conv); err != nil {
			return result{err: err}
		}
		var m map[string]any
		if json.Unmarshal([]byte(raw), &m) != nil || !hasTotal(surface, m) {
			unreplayable++
			continue
		}
		c := provider.UsageConvention(conv)
		if translated != 0 {
			c = provider.UsageConventionAuto // the codec already normalized the body
		}
		n := store.NormalizeUsageWith(store.Surface(surface), m, c)
		checked++
		if n.In != in || n.Out != out || n.CachedRead != cr || n.CachedWrite != cw {
			if len(bad) < 5 {
				bad = append(bad, fmt.Sprintf("row %d (%s): stored in=%d read=%d write=%d out=%d, raw says in=%d read=%d write=%d out=%d",
					id, surface, in, cr, cw, out, n.In, n.CachedRead, n.CachedWrite, n.Out))
			}
		}
	}
	if err := rows.Err(); err != nil {
		return result{err: err}
	}
	lines := []string{fmt.Sprintf("replayed %d rows (%d without a total field skipped)", checked, unreplayable)}
	return result{ok: len(bad) == 0, lines: append(lines, bad...)}
}

// hasTotal reports whether a raw usage object carries the input total the
// normalizer needs; a streamed row's raw may be only its last event.
func hasTotal(surface string, m map[string]any) bool {
	if m == nil {
		return false
	}
	switch surface {
	case "openai":
		_, a := m["prompt_tokens"]
		_, b := m["input_tokens"]
		return a || b
	default:
		_, ok := m["input_tokens"]
		return ok
	}
}

// credentialsEncrypted: every stored provider key (and management key) is an
// enc:v1 envelope. Nothing about a key — not even its length — is printed.
func credentialsEncrypted(db *sql.DB) result {
	var keys, plainKeys, mgmt, plainMgmt int
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN key_plain NOT LIKE 'enc:v1:%' THEN 1 ELSE 0 END),0) FROM provider_keys`).
		Scan(&keys, &plainKeys); err != nil {
		return result{err: err}
	}
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN management_key_plain NOT LIKE 'enc:v1:%' THEN 1 ELSE 0 END),0)
		FROM accounts WHERE COALESCE(management_key_plain,'') <> ''`).Scan(&mgmt, &plainMgmt); err != nil {
		return result{err: err}
	}
	return result{ok: plainKeys == 0 && plainMgmt == 0, lines: []string{
		fmt.Sprintf("provider keys: %d stored, %d not encrypted", keys, plainKeys),
		fmt.Sprintf("management keys: %d stored, %d not encrypted", mgmt, plainMgmt),
	}}
}

func clientTokensHashed(db *sql.DB) result {
	var n, bad int
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN length(token_hash) <> 64 THEN 1 ELSE 0 END),0) FROM client_tokens`).
		Scan(&n, &bad); err != nil {
		return result{err: err}
	}
	return result{ok: bad == 0, lines: []string{fmt.Sprintf("client tokens: %d, %d not a sha256 hex digest", n, bad)}}
}

func rawUsageRetained(db *sql.DB) result {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM calls WHERE raw_usage <> ''`).Scan(&n); err != nil {
		return result{err: err}
	}
	r := result{ok: n > 0, lines: []string{fmt.Sprintf("rows with raw_usage: %d", n)}}
	var raw string
	if db.QueryRow(`SELECT raw_usage FROM calls WHERE raw_usage <> '' ORDER BY id DESC LIMIT 1`).Scan(&raw) == nil {
		r.lines = append(r.lines, "sample: "+trunc(raw, 130))
	}
	return r
}

func streamingIncremental(db *sql.DB) result {
	rows, err := db.Query(`SELECT id, COALESCE(ttft_ms,-1), COALESCE(total_ms,-1) FROM calls
		WHERE stream=1 AND status=200 ORDER BY id DESC LIMIT 3`)
	if err != nil {
		return result{err: err}
	}
	defer rows.Close()
	r := result{ok: true}
	for rows.Next() {
		var id, ttft, total int64
		if err := rows.Scan(&id, &ttft, &total); err != nil {
			return result{err: err}
		}
		ok := ttft > 0 && total > 0 && ttft < total
		r.ok = r.ok && ok
		r.lines = append(r.lines, fmt.Sprintf("row %d: ttft=%dms total=%dms", id, ttft, total))
	}
	if len(r.lines) == 0 {
		r.lines = []string{"no streamed rows yet (nothing to judge)"}
	}
	return r
}

// trunc cuts s to at most n bytes on a rune boundary.
func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimSpace(s[:n]) + "…"
}
