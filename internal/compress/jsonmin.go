package compress

import (
	"bytes"
	"encoding/json"
	"strings"
)

// jsonmin removes the insignificant whitespace from tool output that is one
// whole pretty-printed JSON value (API responses, kubectl -o json, configs).
// It is json.Compact — whitespace between tokens only — so key order,
// duplicate keys, number spellings and string escapes stay byte-identical:
// nothing is decoded and re-encoded. Never on preserve-class output (a file
// read is quoted back by the next edit); log, search and unknown tools all
// qualify. Not strictly smaller (already compact) ⇒ untouched.
func jsonmin(msgs []any, opts map[string]any) []any {
	classes := toolClassOverrides(opts)
	return editAll(msgs, toolNamesByCallID(msgs), func(p textPart) (string, bool) {
		if p.kind != partToolOutput || p.text == "" || !isJSONPayload(p.text) {
			return "", false
		}
		if toolClassFor(p.tool, classes) == toolPreserve {
			return "", false
		}
		var b bytes.Buffer
		if err := json.Compact(&b, []byte(strings.TrimSpace(p.text))); err != nil || b.Len() >= len(p.text) {
			return "", false
		}
		return b.String(), true
	})
}
