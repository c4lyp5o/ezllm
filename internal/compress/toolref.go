package compress

import "strings"

// tool_ref replaces a tool result that is byte-identical to an EARLIER shipped
// tool result with a reference to it — an agent re-reading the same file or
// re-running the same command. The earlier copy stays exactly where it was,
// so the model still has the content; only the NEW message changes, at its
// first sight, and the memo freezes it. That is why it is cache-safe for every
// tool, file reads included (class rules do not apply: nothing the model needs
// leaves the conversation).
//
// Exact matches only, against the SHIPPED text of the earlier copy, so the
// reference is always true about what the model sees. Place it after the
// other tool-output engines (both copies then went through the same ones).
// A reference whose target is no longer in the history is never shipped: the
// pipeline sends that message's original instead (see danglingRefs).
// Option: min_bytes (default 200) — smaller results are not worth a marker.

const (
	refPrefix = "[ezllm/ref: identical to the tool result for call "
	refSuffix = " above]"
)

func refMarkerFor(id string) string { return refPrefix + id + refSuffix }

// refTarget returns the call id a reference marker points at.
func refTarget(text string) (string, bool) {
	if !strings.HasPrefix(text, refPrefix) || !strings.HasSuffix(text, refSuffix) {
		return "", false
	}
	return text[len(refPrefix) : len(text)-len(refSuffix)], true
}

// toolRef runs over the shipped history in order. isNew marks the positions
// it may rewrite (nil = all); every other message is context only. A nil
// entry is a message that ships as nothing.
func toolRef(view []any, isNew []bool, opts map[string]any) []any {
	minBytes := intOption(opts, "min_bytes", 200)
	if minBytes < len(refMarkerFor("")) {
		minBytes = 200
	}
	seen := map[string]string{} // shipped tool text -> id of its first copy
	record := func(m any) {
		editParts(m, nil, func(p textPart) (string, bool) {
			if p.kind == partToolOutput && p.id != "" {
				if _, isRef := refTarget(p.text); !isRef {
					if _, ok := seen[p.text]; !ok {
						seen[p.text] = p.id
					}
				}
			}
			return "", false
		})
	}
	out := make([]any, len(view))
	for i, m := range view {
		if m == nil || (isNew != nil && !isNew[i]) {
			record(m)
			out[i] = m
			continue
		}
		nm, _ := editParts(m, nil, func(p textPart) (string, bool) {
			if p.kind != partToolOutput || len(p.text) < minBytes {
				return "", false
			}
			if id, ok := seen[p.text]; ok && id != p.id {
				return refMarkerFor(id), true
			}
			return "", false
		})
		record(nm)
		out[i] = nm
	}
	return out
}

// toolOutputs maps each call id to the tool output msg carries for it.
func toolOutputs(msg any) map[string]string {
	out := map[string]string{}
	editParts(msg, nil, func(p textPart) (string, bool) {
		if p.kind == partToolOutput && p.id != "" {
			out[p.id] = p.text
		}
		return "", false
	})
	return out
}

// refsResolve reports whether every reference in form (the shipped form of
// orig) points at a call whose real output shipped earlier with exactly the
// content orig holds for the referencing call. Checking only that the target
// id exists is not enough: a memoized reference outlives a history rewrite
// that keeps the id but changes its output, and would then claim text the
// target no longer has. have maps call id -> original output.
func refsResolve(form, orig any, have map[string]string) bool {
	var own map[string]string
	ok := true
	editParts(form, nil, func(p textPart) (string, bool) {
		if id, isRef := refTarget(p.text); isRef && p.kind == partToolOutput {
			if own == nil {
				own = toolOutputs(orig)
			}
			if target, shipped := have[id]; !shipped || target != own[p.id] {
				ok = false
			}
		}
		return "", false
	})
	return ok
}

// realToolIDs records, for each call id form carries real (non-reference)
// output for, the original output orig holds for it.
func realToolIDs(form, orig any, have map[string]string) {
	var own map[string]string
	editParts(form, nil, func(p textPart) (string, bool) {
		if p.kind == partToolOutput && p.id != "" {
			if _, isRef := refTarget(p.text); !isRef {
				if own == nil {
					own = toolOutputs(orig)
				}
				have[p.id] = own[p.id]
			}
		}
		return "", false
	})
}
