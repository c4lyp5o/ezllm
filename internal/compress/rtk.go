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
//
// toolClasses: resolved tool names are filtered BY CLASS. File reads,
// diffs, and anything UNKNOWN are preserved verbatim — unknown must never
// mean "blindly truncate". Only log-class (build/test/terminal) and
// search-class results are head/tail filtered. See toolClassFor.
func rtk(msgs []any, opts map[string]any) []any {
	keep := intOption(opts, "keep_lines", 6)
	if keep < 0 {
		keep = 6
	}
	names := toolNamesByCallID(msgs)
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role != "tool" {
			// Anthropic shape: a tool RESULT is a tool_result block inside a
			// user message's content array — same class gate, same filter.
			if blocks, ok := mm["content"].([]any); ok {
				if filtered, changed := filterToolResultBlocks(blocks, names, keep); changed {
					cp := make(map[string]any, len(mm))
					for k, v := range mm {
						cp[k] = v
					}
					cp["content"] = filtered
					out = append(out, cp)
					continue
				}
			}
			out = append(out, m) // scope: tool_only
			continue
		}
		// Class gate: unknown/unresolved/file/diff classes are preserved.
		if !toolClassFilterable(names[toolCallIDOf(mm)]) {
			out = append(out, m)
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

// toolCallIDOf reads both wire shapes' result ids: OpenAI's tool_call_id and
// Anthropic's tool_use_id.
func toolCallIDOf(mm map[string]any) string {
	if id, _ := mm["tool_call_id"].(string); id != "" {
		return id
	}
	if id, _ := mm["tool_use_id"].(string); id != "" {
		return id
	}
	return ""
}

// toolNamesByCallID maps every tool-call id in the conversation to its
// lowercase tool name: assistant messages' tool_calls[].id +
// .function.name (OpenAI) and content blocks of type tool_use (Anthropic).
// An id absent from the map is an UNKNOWN tool — preserved, not filtered.
func toolNamesByCallID(msgs []any) map[string]string {
	out := map[string]string{}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if calls, ok := mm["tool_calls"].([]any); ok {
			for _, c := range calls {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				id, _ := cm["id"].(string)
				if id == "" {
					continue
				}
				if fn, ok := cm["function"].(map[string]any); ok {
					if name, _ := fn["name"].(string); name != "" {
						out[id] = strings.ToLower(name)
					}
				}
				if tu, ok := cm["tool_use"].(map[string]any); ok {
					if name, _ := tu["name"].(string); name != "" {
						out[id] = strings.ToLower(name)
					}
				}
			}
		}
		if blocks, ok := mm["content"].([]any); ok {
			for _, b := range blocks {
				bm, ok := b.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := bm["type"].(string); t != "tool_use" {
					continue
				}
				id, _ := bm["id"].(string)
				name, _ := bm["name"].(string)
				if id != "" && name != "" {
					out[id] = strings.ToLower(name)
				}
			}
		}
	}
	return out
}

// toolClass is the filtering class of a resolved tool name.
type toolClass int

const (
	toolPreserve toolClass = iota // file reads, diffs, unknown tools
	toolLog                       // build/test/terminal output
	toolSearch                    // ranked results with paths
)

// Bucket membership is a documented policy, not content sniffing. Exact
// name first (so "read" never lands in search's codebase bucket), then
// prefix, then substring.
var (
	preserveToolTokens = []string{"read", "cat", "view", "open", "get_file", "show",
		"edit", "write", "patch", "diff", "apply_patch", "str_replace", "create",
		"insert", "notebook", "move", "rename", "delete", "undo", "commit", "push"}
	logToolTokens = []string{"bash", "shell", "terminal", "exec", "command", "run",
		"test", "pytest", "jest", "vitest", "mocha", "cargo", "npm", "pnpm", "yarn",
		"make", "bundle", "docker", "kubectl", "tail", "system", "script", "build"}
	searchToolTokens = []string{"grep", "glob", "search", "find", "rg", "ls", "list",
		"codebase", "web", "scan", "query"}
)

// toolClassFor classifies a resolved tool name; empty/unknown -> preserve.
func toolClassFor(name string) toolClass {
	if name == "" {
		return toolPreserve
	}
	for _, t := range preserveToolTokens {
		if name == t {
			return toolPreserve
		}
	}
	for _, t := range logToolTokens {
		if name == t {
			return toolLog
		}
	}
	for _, t := range searchToolTokens {
		if name == t {
			return toolSearch
		}
	}
	for _, t := range preserveToolTokens {
		if strings.HasPrefix(name, t) {
			return toolPreserve
		}
	}
	for _, t := range logToolTokens {
		if strings.Contains(name, t) {
			return toolLog
		}
	}
	for _, t := range searchToolTokens {
		if strings.Contains(name, t) {
			return toolSearch
		}
	}
	return toolPreserve
}

// toolClassFilterable reports whether a resolved name may be filtered.
func toolClassFilterable(name string) bool {
	return toolClassFor(name) != toolPreserve
}

// filterToolResultBlocks applies the class gate + unit filter to Anthropic
// tool_result blocks inside a message's content array. Returns the (possibly
// rewritten) block slice and whether anything changed. Blocks whose tool is
// unknown or preserve-class are returned untouched; pairing (tool_use_id)
// is never modified.
func filterToolResultBlocks(blocks []any, names map[string]string, keep int) ([]any, bool) {
	out := make([]any, len(blocks))
	copy(out, blocks)
	changed := false
	for i, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := bm["type"].(string); t != "tool_result" {
			continue
		}
		id, _ := bm["tool_use_id"].(string)
		if !toolClassFilterable(names[id]) {
			continue
		}
		content, isStr := bm["content"].(string)
		if !isStr || content == "" {
			continue
		}
		filtered, ok := filterToolOutput(content, keep)
		if !ok || len(filtered) >= len(content) {
			continue
		}
		cp := make(map[string]any, len(bm))
		for k, v := range bm {
			cp[k] = v
		}
		cp["content"] = filtered
		out[i] = cp
		changed = true
	}
	return out, changed
}
