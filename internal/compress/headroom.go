package compress

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// headroom is the SmartCrusher-style tabular engine (omniroute reference:
// "lossless tabular compaction of homogeneous JSON-array payloads into a
// columnar form"). Every key is paid once instead of once per row.
//
// Scope (same tool_only contract as rtk): role="tool" messages whose trimmed
// content is a pure JSON array of objects sharing ONE key set. Everything
// else — prose, JSON objects, mixed shapes, user/system traffic — passes
// through untouched.
//
// Wire form (one marker line + columnar JSON, both lines the whole content):
//
//	[ezllm/headroom: 3 rows columnar, lossless — row i = i-th element of each column]
//	{"id":[1,2,3],"name":["a","b","c"]}
//
// Lossless by construction: the columns hold every value, so a reader zips
// the columns to recover each row (key order may differ — JSON objects are
// unordered; values are exact, numbers round-trip via json.Number).
//
// Skips (keep the original bytes) when: not a JSON array, rows are not all
// objects, key sets differ across rows, fewer than min_rows rows (default 4),
// or the columnar encoding is not strictly smaller than the original.
func headroom(msgs []any, opts map[string]any) []any {
	minRows := intOption(opts, "min_rows", 4)
	if minRows < 1 {
		minRows = 1
	}
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role != "tool" {
			out = append(out, m) // scope: tool_only
			continue
		}
		content, isStr := mm["content"].(string)
		if !isStr || content == "" {
			out = append(out, m)
			continue
		}
		compacted, ok := compactTable(content, minRows)
		if !ok || len(compacted) >= len(content) {
			out = append(out, m) // no gain: keep the original bytes
			continue
		}
		cp := make(map[string]any, len(mm))
		for k, v := range mm {
			cp[k] = v
		}
		cp["content"] = compacted
		out = append(out, cp)
	}
	return out
}

// compactTable rewrites a homogeneous JSON-array payload into the columnar
// wire form. ok=false = leave this content alone (see skip rules above).
func compactTable(content string, minRows int) (string, bool) {
	trimmed := content
	if !json.Valid([]byte(trimmed)) {
		return "", false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber() // exact number round-trip (64-bit ids, decimals)
	var rows []any
	if err := dec.Decode(&rows); err != nil || len(rows) == 0 {
		return "", false // not a JSON array
	}
	// Consume trailing whitespace-only remainder; anything else means the
	// content is not a single pure array.
	if dec.More() {
		return "", false
	}
	if len(rows) < minRows {
		return "", false
	}

	var keys []string
	for i, r := range rows {
		obj, isObj := r.(map[string]any)
		if !isObj || len(obj) == 0 {
			return "", false // tabular needs object rows with columns
		}
		if i == 0 {
			for k := range obj {
				keys = append(keys, k)
			}
			sort.Strings(keys) // deterministic wire form
		} else if len(obj) != len(keys) {
			return "", false // ragged rows: not a table
		} else {
			for _, k := range keys {
				if _, ok := obj[k]; !ok {
					return "", false // key sets differ across rows
				}
			}
		}
	}

	cols := make(map[string]any, len(keys))
	for _, k := range keys {
		col := make([]any, len(rows))
		for i, r := range rows {
			col[i] = r.(map[string]any)[k]
		}
		cols[k] = col
	}
	body, err := json.Marshal(cols)
	if err != nil {
		return "", false
	}
	marker := fmt.Sprintf("[ezllm/headroom: %d rows columnar, lossless — row i = i-th element of each column]",
		len(rows))
	return marker + "\n" + string(body), true
}
