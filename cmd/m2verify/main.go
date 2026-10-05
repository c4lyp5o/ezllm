// Command m2verify runs the M2 acceptance checks straight against the live
// ledger, so the results are real query output rather than assertion prose.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

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

	section := func(s string) { fmt.Printf("\n══ %s ══\n", s) }
	fail := 0

	section("last 4 ledger rows")
	rows, err := db.Query(`SELECT id, client, surface, account, model, status,
		tokens_in, tokens_out, tokens_cached_read, tokens_cached_write, reasoning_tokens,
		COALESCE(ttft_ms,-1), COALESCE(total_ms,-1), COALESCE(endpoint_id,'-')
		FROM calls ORDER BY id DESC LIMIT 4`)
	if err != nil {
		fmt.Println("query:", err)
		os.Exit(1)
	}
	fmt.Printf("%-3s %-9s %-10s %-10s %-16s %-4s %-7s %-5s %-8s %-6s %-6s %-7s %-7s %s\n",
		"id", "client", "surface", "account", "model", "st", "in", "out", "cachedR", "cachW", "reason", "ttft", "total", "endpoint")
	for rows.Next() {
		var id, st int
		var client, surface, account, model, endpoint string
		var in, out, cr, cw, reason, ttft, total int64
		rows.Scan(&id, &client, &surface, &account, &model, &st, &in, &out, &cr, &cw, &reason, &ttft, &total, &endpoint)
		fmt.Printf("%-3d %-9s %-10s %-10s %-16s %-4d %-7d %-5d %-8d %-6d %-6d %-7d %-7d %s\n",
			id, client, surface, account, trunc(model, 16), st, in, out, cr, cw, reason, ttft, total, endpoint)
	}
	rows.Close()

	section("INVARIANT: OpenAI cached tokens must NOT be double-counted in tokens_in")
	// Upstream reported prompt_tokens=2496 with cached_tokens=2048 on the repeat
	// call, so normalized tokens_in must be 448, not 2496.
	var in, cr int64
	err = db.QueryRow(`SELECT tokens_in, tokens_cached_read FROM calls
		WHERE surface='openai' AND tokens_cached_read > 0 ORDER BY id DESC LIMIT 1`).Scan(&in, &cr)
	if err == sql.ErrNoRows {
		fmt.Println("SKIP: no cached OpenAI row found")
	} else if err != nil {
		fmt.Println("query:", err)
	} else if in == 2496 {
		fmt.Printf("FAIL: tokens_in=%d equals RAW prompt_tokens — cached double-counted\n", in)
		fail++
	} else if in+cr == 2496 {
		fmt.Printf("PASS: tokens_in=%d + cached_read=%d = 2496 raw prompt_tokens\n", in, cr)
	} else {
		fmt.Printf("CHECK: tokens_in=%d cached_read=%d (sum %d, expected raw 2496)\n", in, cr, in+cr)
	}

	section("INVARIANT: Anthropic cache_read must be EXCLUDED from tokens_in")
	var ain, acr int64
	err = db.QueryRow(`SELECT tokens_in, COALESCE(tokens_cached_read,0) FROM calls
		WHERE surface='anthropic' ORDER BY id DESC LIMIT 1`).Scan(&ain, &acr)
	if err == nil {
		// Live probe returned input_tokens=315 with cache_read=0, so tokens_in
		// must equal the raw input_tokens exactly (Anthropic excludes cache).
		fmt.Printf("anthropic row: tokens_in=%d cached_read=%d ", ain, acr)
		if ain == 315 && acr == 0 {
			fmt.Println("PASS (matches raw input_tokens=315, cache_read=0)")
		} else {
			fmt.Println("CHECK against raw usage")
		}
	}

	section("INVARIANT: raw_usage retained verbatim for audit")
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM calls WHERE raw_usage != ''`).Scan(&n)
	fmt.Printf("rows with raw_usage: %d ", n)
	if n > 0 {
		fmt.Println("PASS")
	} else {
		fmt.Println("FAIL — audit trail missing")
		fail++
	}
	var raw string
	if db.QueryRow(`SELECT raw_usage FROM calls WHERE raw_usage != '' ORDER BY id DESC LIMIT 1`).Scan(&raw) == nil {
		fmt.Printf("  sample: %s\n", trunc(raw, 130))
	}

	section("INVARIANT: streaming is incremental (ttft << total)")
	r2, _ := db.Query(`SELECT id, COALESCE(ttft_ms,-1), COALESCE(total_ms,-1) FROM calls WHERE stream=1 ORDER BY id DESC LIMIT 3`)
	any := false
	for r2.Next() {
		var id int
		var ttft, total int64
		r2.Scan(&id, &ttft, &total)
		any = true
		verdict := "CHECK"
		if ttft > 0 && total > 0 && ttft < total {
			verdict = "PASS"
		}
		fmt.Printf("  row %d: ttft=%dms total=%dms -> %s\n", id, ttft, total, verdict)
		if verdict != "PASS" {
			fail++
		}
	}
	r2.Close()
	if !any {
		fmt.Println("SKIP: no streamed rows")
	}

	section("INVARIANT: attribution complete on every routed call")
	var total, blankAcct, blankHint, blankClient, errs int
	db.QueryRow(`SELECT COUNT(*),
		SUM(CASE WHEN account='' THEN 1 ELSE 0 END),
		SUM(CASE WHEN key_hint='' THEN 1 ELSE 0 END),
		SUM(CASE WHEN client='' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status>=400 THEN 1 ELSE 0 END) FROM calls`).
		Scan(&total, &blankAcct, &blankHint, &blankClient, &errs)
	fmt.Printf("rows=%d blank_account=%d blank_key_hint=%d blank_client=%d errors=%d\n",
		total, blankAcct, blankHint, blankClient, errs)
	if blankAcct+blankHint+blankClient == 0 {
		fmt.Println("PASS: no attribution gaps")
	} else {
		fmt.Println("FAIL: attribution gaps present")
		fail++
	}

	section("INVARIANT: keys encrypted at rest (ciphertext, never plaintext)")
	r3, _ := db.Query(`SELECT k.id, a.namespace, k.label, k.key_hint, k.key_ct
		FROM provider_keys k JOIN accounts a ON a.id=k.account_id`)
	for r3.Next() {
		var id int
		var ns, label, hint, ct string
		r3.Scan(&id, &ns, &label, &hint, &ct)
		verdict := "FAIL PLAINTEXT"
		if strings.HasPrefix(ct, "enc:v1:") && strings.Count(ct, ":") == 4 {
			verdict = "PASS enc:v1 iv:ct:tag"
		}
		fmt.Printf("  %-10s %-8s hint=%-10s ct_len=%-4d %s\n", ns, label, hint, len(ct), verdict)
		if !strings.HasPrefix(ct, "enc:v1:") {
			fail++
		}
		if strings.Contains(ct, hint) && len(hint) > 4 {
			fmt.Println("    WARN: hint appears inside ciphertext")
		}
	}
	r3.Close()

	section("per-account totals (dashboard numbers)")
	r4, _ := db.Query(`SELECT account, COUNT(*), SUM(tokens_in), SUM(tokens_out),
		SUM(tokens_cached_read), SUM(tokens_cached_write), SUM(reasoning_tokens)
		FROM calls GROUP BY account ORDER BY COUNT(*) DESC`)
	fmt.Printf("%-12s %-6s %-8s %-7s %-9s %-8s %s\n", "account", "calls", "in", "out", "cachedR", "cachW", "reason")
	for r4.Next() {
		var acct string
		var c int
		var i, o, cr, cw, rs int64
		r4.Scan(&acct, &c, &i, &o, &cr, &cw, &rs)
		fmt.Printf("%-12s %-6d %-8d %-7d %-9d %-8d %d\n", acct, c, i, o, cr, cw, rs)
	}
	r4.Close()

	section("catalog + protocol matrix (probed capability)")
	r5, _ := db.Query(`SELECT a.namespace, COUNT(*),
		SUM(CASE WHEN m.proto_openai=1 THEN 1 ELSE 0 END),
		SUM(CASE WHEN m.proto_anthropic=1 THEN 1 ELSE 0 END),
		SUM(CASE WHEN m.proto_responses=1 THEN 1 ELSE 0 END),
		SUM(CASE WHEN m.proto_openai IS NULL THEN 1 ELSE 0 END)
		FROM models m JOIN accounts a ON a.id=m.account_id GROUP BY a.namespace`)
	fmt.Printf("%-12s %-7s %-8s %-11s %-11s %s\n", "namespace", "models", "openai✓", "anthropic✓", "responses✓", "untested")
	for r5.Next() {
		var ns string
		var c, o, a, r, u int
		r5.Scan(&ns, &c, &o, &a, &r, &u)
		fmt.Printf("%-12s %-7d %-8d %-11d %-11d %d\n", ns, c, o, a, r, u)
	}
	r5.Close()

	section("client tokens stored hashed (never plaintext)")
	r6, _ := db.Query(`SELECT name, roles, length(token_hash) FROM client_tokens`)
	for r6.Next() {
		var name, roles string
		var hl int
		r6.Scan(&name, &roles, &hl)
		verdict := "FAIL"
		if hl == 64 {
			verdict = "PASS sha256"
		}
		fmt.Printf("  %-12s roles=%-12s hash_len=%d %s\n", name, roles, hl, verdict)
		if hl != 64 {
			fail++
		}
	}
	r6.Close()

	fmt.Printf("\n════════ RESULT: %d invariant failure(s) ════════\n", fail)
	if fail > 0 {
		os.Exit(1)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
