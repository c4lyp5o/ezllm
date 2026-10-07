package compress

// M6.5 phase 2: headroom (lossless columnar JSON) + lite (whitespace-only
// cleanup). The load-bearing property of BOTH engines: nothing that reaches
// the model loses meaning. headroom is lossless by construction (zip the
// columns back), lite never touches words or fenced bytes.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func toolMsg(content string) map[string]any {
	return map[string]any{"role": "tool", "content": content}
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }
func pad3(i int) string { return fmt.Sprintf("%03d", i) }

func mustJSONString(s string) string { return string(mustMarshal(s)) }

// reconstruct undoes the columnar wire form and returns the rows it encodes,
// so tests assert losslessness against the ORIGINAL payload.
func reconstruct(t *testing.T, compacted string) []any {
	t.Helper()
	lines := strings.SplitN(compacted, "\n", 2)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "[ezllm/headroom:") {
		t.Fatalf("not a headroom wire form: %q", compacted)
	}
	dec := json.NewDecoder(strings.NewReader(lines[1]))
	dec.UseNumber()
	var cols map[string]any
	if err := dec.Decode(&cols); err != nil {
		t.Fatalf("columnar body is not JSON: %v", err)
	}
	var n int
	var keys []string
	for k, v := range cols {
		col, ok := v.([]any)
		if !ok {
			t.Fatalf("column %q is not an array", k)
		}
		keys = append(keys, k)
		if n == 0 {
			n = len(col)
		} else if len(col) != n {
			t.Fatalf("ragged columns: %q has %d, want %d", k, len(col), n)
		}
	}
	rows := make([]any, n)
	for i := range rows {
		row := map[string]any{}
		for _, k := range keys {
			row[k] = cols[k].([]any)[i]
		}
		rows[i] = row
	}
	return rows
}

func decodeJSON(t *testing.T, s string) []any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var out []any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestHeadroomColumnarLossless(t *testing.T) {
	payload := `[
		{"id":1,"name":"inventory-row-001-alpha","score":9},
		{"id":2,"name":"inventory-row-002-bravo","score":7},
		{"id":3,"name":"inventory-row-003-charlie","score":8},
		{"id":4,"name":"inventory-row-004-delta","score":6},
		{"id":5,"name":"inventory-row-005-echo","score":7}
	]`
	out := headroom([]any{toolMsg(payload)}, nil)
	got := out[0].(map[string]any)["content"].(string)

	if got == payload {
		t.Fatal("payload was not compacted")
	}
	if len(got) >= len(payload) {
		t.Fatalf("compaction grew the payload: %d -> %d", len(payload), len(got))
	}
	if !strings.Contains(strings.Split(got, "\n")[0], "5 rows") {
		t.Errorf("marker line missing row count: %q", strings.Split(got, "\n")[0])
	}

	// The heart of the engine: what comes back must equal what went in.
	want := decodeJSON(t, payload)
	rec := reconstruct(t, got)
	if !reflect.DeepEqual(rec, want) {
		t.Errorf("round-trip mismatch:\n want %v\n  got %v", want, rec)
	}
}

func TestHeadroomExactNumbers(t *testing.T) {
	// 64-bit ids must not take the float64 round trip. Encoder-level on
	// purpose: these tiny payloads never pass the no-gain gate (their
	// marker would cost more than the savings) and must not need to.
	payload := `[{"uid":9007199254740993},{"uid":9007199254740995},{"uid":9007199254740997},{"uid":9007199254740999}]`
	got, ok := compactTable(payload, 4)
	if !ok {
		t.Fatal("encoder refused a homogeneous 4-row payload")
	}
	rec := reconstruct(t, got)
	want := decodeJSON(t, payload)
	if !reflect.DeepEqual(rec, want) {
		t.Errorf("numbers corrupted:\n want %v\n  got %v", want, rec)
	}
}

func TestHeadroomSkips(t *testing.T) {
	tests := []struct {
		name    string
		role    string
		content string
	}{
		{"ragged keys", "tool", `[{"a":1,"b":2},{"a":3,"c":4},{"a":5,"b":6},{"a":7,"b":8}]`},
		{"scalar rows", "tool", `[1,2,3,4]`},
		{"scalar array", "tool", `["x","y","z","w"]`},
		{"json object", "tool", `{"rows":[{"a":1},{"a":2},{"a":3},{"a":4}]}`},
		{"below min_rows", "tool", `[{"a":1},{"a":2}]`},
		{"empty object rows", "tool", `[{},{},"x"]`},
		{"prose", "tool", "just plain tool text, not json at all"},
		{"scope: user role", "user", `[{"a":1},{"a":2},{"a":3},{"a":4}]`},
		{"scope: system role", "system", `[{"a":1},{"a":2},{"a":3},{"a":4}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []any{map[string]any{"role": tc.role, "content": tc.content}}
			out := headroom(msgs, nil)
			if got := out[0].(map[string]any)["content"].(string); got != tc.content {
				t.Errorf("content changed:\n in  %q\n out %q", tc.content, got)
			}
		})
	}
}

func TestHeadroomNoGainKeepsOriginal(t *testing.T) {
	// 4 rows (meets min_rows) whose values dwarf the repeated keys: the
	// marker + columnar wrap costs more than it saves -> original bytes.
	payload := `[{"k":"` + strings.Repeat("a", 200) + `"},{"k":"` + strings.Repeat("b", 200) +
		`"},{"k":"` + strings.Repeat("c", 200) + `"},{"k":"` + strings.Repeat("d", 200) + `"}]`
	out := headroom([]any{toolMsg(payload)}, nil)
	if got := out[0].(map[string]any)["content"].(string); got != payload {
		t.Error("payload rewritten despite no savings")
	}
}

func TestHeadroomMinRowsOption(t *testing.T) {
	payload := `[{"a":1},{"a":2}]`
	if _, ok := compactTable(payload, 4); ok {
		t.Error("min_rows=4 should refuse a 2-row payload")
	}
	got, ok := compactTable(payload, 2)
	if !ok {
		t.Fatal("min_rows=2 should accept a 2-row payload")
	}
	if !reflect.DeepEqual(reconstruct(t, got), decodeJSON(t, payload)) {
		t.Error("round-trip mismatch under min_rows option")
	}
}

func TestLiteWhitespaceOnly(t *testing.T) {
	in := "\n\nLead blank removed.   \nTrailing spaces die.   \n\n\n\nSecond paragraph.\n"
	msgs := []any{
		map[string]any{"role": "user", "content": in},
	}
	out := lite(msgs, nil)
	got := out[0].(map[string]any)["content"].(string)
	want := "Lead blank removed.\nTrailing spaces die.\n\nSecond paragraph."
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestLiteKeepsFencesByteIdentical(t *testing.T) {
	in := "intro   \n```python\nx = 1   \n\n\ny = 2   \n```\noutro   \n\n\n\n"
	out := lite([]any{map[string]any{"role": "assistant", "content": in}}, nil)
	got := out[0].(map[string]any)["content"].(string)

	if !strings.Contains(got, "x = 1   \n\n\ny = 2   ") {
		t.Errorf("fence interior was modified:\n%s", got)
	}
	if !strings.HasPrefix(got, "intro\n") || !strings.HasSuffix(got, "outro") {
		t.Errorf("outside-fence whitespace not cleaned:\n%q", got)
	}
}

func TestLiteSkipsPureJSONPayloads(t *testing.T) {
	payload := "{\n\n  \"a\": 1,\n\n  \"b\": 2\n\n}"
	out := lite([]any{toolMsg(payload)}, nil)
	if got := out[0].(map[string]any)["content"].(string); got != payload {
		t.Errorf("JSON payload touched by lite: %q", got)
	}
}

func TestLiteUnclosedFenceSkips(t *testing.T) {
	payload := "line   \n```\ncode   \n"
	out := lite([]any{map[string]any{"role": "tool", "content": payload}}, nil)
	if got := out[0].(map[string]any)["content"].(string); got != payload {
		t.Errorf("unclosed fence content was rewritten: %q", got)
	}
}

func TestLiteNoChangeKeepsOriginal(t *testing.T) {
	payload := "already\nclean\ntext"
	out := lite([]any{map[string]any{"role": "user", "content": payload}}, nil)
	if got := out[0].(map[string]any)["content"].(string); got != payload {
		t.Errorf("clean content rewritten: %q", got)
	}
}

func TestLiteBlankLinesOption(t *testing.T) {
	in := "a\n\n\n\n\nb"
	out := lite([]any{map[string]any{"role": "user", "content": in}},
		map[string]any{"blank_lines": 2})
	got := out[0].(map[string]any)["content"].(string)
	if got != "a\n\n\nb" {
		t.Errorf("blank_lines=2 produced %q, want %q", got, "a\n\n\nb")
	}
}

// End-to-end through Apply: the profile records real savings and the
// integrity gate still passes with both engines stacked.
func TestApplyHeadroomAndLite(t *testing.T) {
	// 10 rows: big enough that the columnar form beats the teaching marker
	// (smaller payloads are correctly refused by the no-gain gate).
	rows := make([]string, 10)
	for i := range rows {
		rows[i] = `{"id":` + itoa(i+1) + `,"name":"inventory-row-` + pad3(i+1) + `","qty":` + itoa((i+1)*3) + `}`
	}
	payload := "[" + strings.Join(rows, ",") + "]"
	body := []byte(`{"model":"x","messages":[
		{"role":"user","content":"Prose with trailing spaces.   \n\n\n\n"},
		{"role":"tool","content":` + mustJSONString(payload) + `}
	]}`)
	p := &Profile{Name: "p2", Enabled: true, Stages: []Stage{
		{Engine: "headroom"}, {Engine: "lite"},
	}}
	out, res := Apply(body, p)
	if !res.Applied {
		t.Fatalf("not applied: %s", res.Err)
	}
	if res.Saved <= 0 {
		t.Errorf("Saved = %d, want > 0", res.Saved)
	}
	if len(out) >= len(body) {
		t.Errorf("body not smaller: %d -> %d", len(body), len(out))
	}
	var root struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if !strings.HasPrefix(root.Messages[1]["content"].(string), "[ezllm/headroom:") {
		t.Errorf("tool content not compacted: %q", root.Messages[1]["content"])
	}
	if strings.HasSuffix(root.Messages[0]["content"].(string), "   ") {
		t.Error("user prose trailing spaces survived lite")
	}
}
