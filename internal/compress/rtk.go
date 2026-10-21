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
	overrides := toolClassOverrides(opts)
	// One rule for every shape (parts.go): tool output whose resolved tool
	// is log- or search-class, never JSON, filtered only when it shrinks.
	return editAll(msgs, toolNamesByCallID(msgs), func(p textPart) (string, bool) {
		if p.kind != partToolOutput || p.text == "" || isJSONPayload(p.text) {
			return "", false
		}
		if c := toolClassFor(p.tool, overrides); c != toolLog && c != toolSearch {
			return "", false
		}
		filtered, ok := filterToolOutput(p.text, keep)
		if !ok || len(filtered) >= len(p.text) {
			return "", false // integrity skip or no gain
		}
		return filtered, true
	})
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
		// Skip the separator and an optional "code"/"status" word. (This was
		// TrimLeft with a cutset of those letters, which also ate the start
		// of unrelated words.)
		tail := strings.TrimLeft(ls[min(i+4, len(ls)):], " :")
		for _, w := range []string{"code", "status"} {
			tail = strings.TrimPrefix(tail, w)
		}
		tail = strings.TrimLeft(tail, " :")
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

// toolCallIDOf reads a tool-result message's id: chat's tool_call_id (and
// tool_use_id on messages that carry it at top level).
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
// .function.name (chat), content blocks of type tool_use (messages), and
// function_call items (responses).
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
		// Responses API: a function_call item names the tool for its call_id.
		if t, _ := mm["type"].(string); t == "function_call" {
			id, _ := mm["call_id"].(string)
			name, _ := mm["name"].(string)
			if id != "" && name != "" {
				out[id] = strings.ToLower(name)
			}
			continue
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
	// toolPreserve is VERBATIM to every engine: file reads, edits and diffs
	// (an agent's next edit quotes this text back and must match it
	// exactly), and anything a profile marks "preserve".
	toolPreserve toolClass = iota
	toolLog                // build/test/terminal output
	toolSearch             // ranked results with paths
	// toolUnknown is an unrecognised tool: never truncated (rtk leaves it
	// alone), but lossless engines (lite, headroom) may still tidy it.
	toolUnknown
)

// Classification is a documented policy, not content sniffing, applied in
// this order (first match wins):
//
//  1. the profile's tool_classes overrides (rtk stage option) — how an app
//     names ITS tools is the operator's knowledge, not a guess baked in here;
//  2. exact built-in names for widely used tools;
//  3. prefixes (read_*, run_*, …);
//  4. substrings — only long, distinctive tokens: a short token matched
//     inside a name misfires ("rg" in merge_pr, "ls" in list_tools, "web" in
//     webfetch) and head/tail-filters content that is not a log;
//  5. unknown: never truncated (rtk skips it); lossless engines may tidy it.
var (
	preserveExact = toolSet("read", "read_file", "view", "cat", "open", "show", "get_file",
		"edit", "edit_file", "multiedit", "write", "write_file", "create", "insert",
		"patch", "apply_patch", "diff", "str_replace", "str_replace_editor",
		"notebook", "notebookedit", "notebookread", "move", "rename", "delete", "undo",
		"commit", "push", "fetch", "webfetch", "web_fetch", "task", "todowrite", "todoread")
	logExact = toolSet("bash", "bashoutput", "shell", "terminal", "exec", "command",
		"run", "run_command", "run_terminal_cmd", "execute_command", "test", "pytest", "jest",
		"vitest", "mocha", "cargo", "npm", "pnpm", "yarn", "make", "bundle", "docker",
		"kubectl", "tail", "system", "script", "build")
	searchExact = toolSet("grep", "glob", "search", "find", "rg", "ls", "list", "list_dir",
		"list_files", "codebase_search", "file_search", "websearch", "web_search", "scan", "query")

	preservePrefixes = []string{"read", "view", "open", "get_file", "show", "edit", "write",
		"patch", "diff", "apply_patch", "str_replace", "create", "insert", "notebook",
		"move", "rename", "delete", "undo", "commit", "push", "fetch", "web_fetch"}
	logPrefixes     = []string{"run", "exec", "npm", "pnpm", "yarn", "make", "test"}
	logSubstrings   = []string{"bash", "shell", "terminal", "command", "pytest", "vitest", "mocha", "cargo", "docker", "kubectl"}
	searchSubstring = []string{"grep", "glob", "search"}
)

func toolSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// toolClassFor classifies a resolved (lowercase) tool name; overrides come
// from the rtk stage's tool_classes option and may be nil.
func toolClassFor(name string, overrides map[string]toolClass) toolClass {
	if name == "" {
		return toolUnknown
	}
	if c, ok := overrides[name]; ok {
		return c
	}
	switch {
	case preserveExact[name]:
		return toolPreserve
	case logExact[name]:
		return toolLog
	case searchExact[name]:
		return toolSearch
	}
	for _, t := range preservePrefixes {
		if strings.HasPrefix(name, t) {
			return toolPreserve
		}
	}
	for _, t := range logPrefixes {
		if strings.HasPrefix(name, t) {
			return toolLog
		}
	}
	for _, t := range logSubstrings {
		if strings.Contains(name, t) {
			return toolLog
		}
	}
	for _, t := range searchSubstring {
		if strings.Contains(name, t) {
			return toolSearch
		}
	}
	return toolUnknown
}

// toolClassNames are the values the tool_classes option accepts.
var toolClassNames = map[string]toolClass{"log": toolLog, "search": toolSearch, "preserve": toolPreserve}

// toolClassOverrides reads the rtk stage's tool_classes option:
// {"<tool name>": "log"|"search"|"preserve"}. Names match case-insensitively;
// an unknown class value is ignored (admin validation rejects it on write).
func toolClassOverrides(opts map[string]any) map[string]toolClass {
	raw, ok := opts["tool_classes"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[string]toolClass, len(raw))
	for name, v := range raw {
		s, _ := v.(string)
		if c, ok := toolClassNames[strings.ToLower(strings.TrimSpace(s))]; ok {
			out[strings.ToLower(strings.TrimSpace(name))] = c
		}
	}
	return out
}

// ValidToolClass reports whether v is an accepted tool_classes value.
func ValidToolClass(v string) bool {
	_, ok := toolClassNames[strings.ToLower(strings.TrimSpace(v))]
	return ok
}
