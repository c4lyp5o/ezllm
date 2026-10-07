package compress

import (
	"encoding/json"
	"strconv"
	"strings"
)

// rtk filters terminal/tool output — scope: tool_only, never user prose.
// Contract: never mid-JSON (JSON content skipped whole), fenced regions are
// atomic (never split), error lines always win.
//
// Unit algorithm (docs/compression-design.md):
//  1. skip JSON content
//  2. split into units: plain lines + fenced regions (atomic)
//  3. keep head keep_lines + tail keep_lines + every error-signature unit
//  4. drop the middle; each contiguous run -> one `[ezllm/rtk: N lines elided]`
//  5. if not strictly smaller, keep the original
func rtk(msgs []any, opts map[string]any) []any {
	keep := intOption(opts, "keep_lines", 6)
	if keep < 0 {
		keep = 6
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
		trimmed := strings.TrimSpace(content)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			if json.Valid([]byte(trimmed)) {
				out = append(out, m) // never mid-JSON
				continue
			}
		}
		filtered, ok := filterToolOutput(content, keep)
		if !ok || len(filtered) >= len(content) {
			out = append(out, m) // integrity skip or no gain
			continue
		}
		cp := make(map[string]any, len(mm))
		for k, v := range mm {
			cp[k] = v
		}
		cp["content"] = filtered
		out = append(out, cp)
	}
	return out
}

// filterToolOutput applies the unit algorithm. ok=false means "skip this
// content" (unclosed fence / degenerate input).
func filterToolOutput(content string, keep int) (string, bool) {
	units, ok := splitUnits(content)
	if !ok || len(units) == 0 {
		return content, false
	}
	n := len(units)
	keepAt := make([]bool, n)
	for i := 0; i < n; i++ {
		if i < keep || i >= n-keep || isErrorUnit(units[i]) {
			keepAt[i] = true
		}
	}
	// Count drops; if nothing drops, no rewrite.
	drops := 0
	for _, k := range keepAt {
		if !k {
			drops++
		}
	}
	if drops == 0 {
		return content, false
	}

	var b strings.Builder
	b.Grow(len(content))
	i := 0
	for i < n {
		if keepAt[i] {
			b.WriteString(units[i].text)
			b.WriteByte('\n')
			i++
			continue
		}
		// contiguous dropped run -> one marker
		run := 0
		for i < n && !keepAt[i] {
			run++
			i++
		}
		b.WriteString("[ezllm/rtk: ")
		b.WriteString(strconv.Itoa(run))
		b.WriteString(" lines elided]\n")
	}
	return strings.TrimRight(b.String(), "\n"), true
}

// unit is one atomic piece of output: a plain line or a whole fenced region.
type unit struct {
	text   string
	inside bool // true for fence-region units (classified as one)
}

// splitUnits splits content into lines, grouping ``` fenced regions as single
// units. ok=false on an unclosed fence (integrity rule: skip content).
func splitUnits(content string) ([]unit, bool) {
	lines := strings.Split(content, "\n")
	var units []unit
	inFence := false
	var fence []string
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		isFenceMarker := strings.HasPrefix(trimmed, "```")
		if inFence {
			fence = append(fence, ln)
			if isFenceMarker {
				units = append(units, unit{text: strings.Join(fence, "\n"), inside: true})
				fence = nil
				inFence = false
			}
			continue
		}
		if isFenceMarker {
			inFence = true
			fence = []string{ln}
			continue
		}
		units = append(units, unit{text: ln})
	}
	if inFence {
		return nil, false // unclosed fence: skip this content entirely
	}
	return units, true
}

// isErrorUnit: the line that must survive — build/CLI error signatures.
func isErrorUnit(u unit) bool {
	s := u.text
	ls := strings.ToLower(s)
	for _, sig := range []string{
		"error", "failed", "fatal", "panic", "exception", "traceback",
		"err!", "cannot ", "undefined reference", "no such file",
		"permission denied", "killed", "segfault",
	} {
		if strings.Contains(ls, sig) {
			return true
		}
	}
	// exit codes: "exit code 1", "exit status 1", "exit: 1"
	if i := strings.Index(ls, "exit"); i >= 0 {
		tail := ls[min(i+4, len(ls)):]
		tail = strings.TrimLeft(tail, " :code status")
		if tail != "" && tail[0] >= '1' && tail[0] <= '9' {
			return true
		}
	}
	return false
}

func intOption(opts map[string]any, key string, def int) int {
	if opts == nil {
		return def
	}
	switch v := opts[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i)
		}
	}
	return def
}
