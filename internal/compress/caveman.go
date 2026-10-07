package compress

// ── caveman: prose condensation (scope: text_only) ─────────────────────────
// The only lossy engine. It condenses VERBOSE REQUEST PROSE using deterministic
// rule packs — never a model, never a tokenizer — so the same input always
// produces the same output and every rewrite is auditable.
//
// Safety is layered, and the layers are checked in the order that makes failure
// cheap:
//
//  1. scope: only `content` strings on plain text messages. role:"tool" is
//     headroom/rtk territory; assistant tool_calls are never read.
//  2. whole-content skip: JSON content, or any content with an unclosed fence,
//     is left completely alone (splitUnits returns ok=false).
//  3. fenced regions are atomic and skipped whole — code survives verbatim even
//     when it sits inside a prose message.
//  4. inline protection: paths, URLs, identifiers, quoted strings, numbers and
//     versions are lifted out before any rule runs and restored byte-identical.
//  5. span-integrity gate: every protected span must still be present, in
//     order. One missing ⇒ discard the rewrite, keep the original.
//  6. emptied-segment guard: a prose segment that carried words cannot be
//     reduced to nothing. This catches an over-broad rule eating a whole
//     sentence. (A message-level word-loss CEILING was tried and rejected: it
//     cannot tell "removed five filler words" from "ate a paragraph", so it
//     false-rejected exactly the verbose prompts worth compressing. Segment
//     granularity plus span integrity is the honest signal.)
//  7. no strict byte reduction ⇒ skip.
//
// Then the profile-level min_compress_ratio floor applies as usual.
//
// exempt_last_turn (contract rule 1) is enforced by Apply, not here — but it is
// the single most important gate for this engine: the user's live question is
// never condensed.

import (
	"encoding/json"
	"strings"
	"sync"
)

// caveman packs are parsed once, at first use, and shared. They are read-only
// after that, so a plain sync.Once + immutable slice is enough.
var (
	cavemanOnce  sync.Once
	cavemanPacks []cavemanPack
	cavemanDiags []string
)

func loadCavemanPacks() ([]cavemanPack, []string) {
	cavemanOnce.Do(func() {
		cavemanPacks, cavemanDiags = parsePacks(embeddedPacks())
	})
	return cavemanPacks, cavemanDiags
}

// CavemanDiags exposes pack-load diagnostics (rejected packs and why). Tests and
// the admin surface read it; a rejected pack must never be silent.
func CavemanDiags() []string {
	_, diags := loadCavemanPacks()
	return diags
}

// caveman condenses prose in text messages. Returns the rewritten messages and
// the number of rules that fired (for the ledger's attribution column).
func caveman(msgs []any, opts map[string]any) ([]any, int) {
	packs, _ := loadCavemanPacks()
	if len(packs) == 0 {
		return msgs, 0 // no valid packs: fail-open, untouched
	}

	intensity := intensityLite
	if s, ok := opts["intensity"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "standard", "aggressive", "ultra":
			intensity = intensityStandard
		case "lite", "":
			intensity = intensityLite
		default:
			return msgs, 0 // unknown level: fail-open rather than guess
		}
	}

	firedTotal := 0
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		obj, isObj := m.(map[string]any)
		if !isObj {
			out = append(out, m)
			continue
		}
		role, _ := obj["role"].(string)
		// text_only: system prompts are instructions we must not paraphrase,
		// tool results belong to headroom/rtk, and assistant tool_calls are
		// never read. Only user prose and assistant prose are candidates — and
		// assistant prose is history at this point (the live turn is exempt).
		if role != "user" && role != "assistant" {
			out = append(out, m)
			continue
		}
		content, isStr := obj["content"].(string)
		if !isStr || strings.TrimSpace(content) == "" {
			out = append(out, m)
			continue
		}

		condensed, fired, ok := condenseProse(content, packs, intensity)
		if !ok || condensed == content {
			out = append(out, m)
			continue
		}
		// Copy-on-write: never mutate the caller's map. Only `content` changes.
		clone := make(map[string]any, len(obj))
		for k, v := range obj {
			clone[k] = v
		}
		clone["content"] = condensed
		out = append(out, clone)
		firedTotal += fired
	}
	if firedTotal == 0 {
		return msgs, 0
	}
	return out, firedTotal
}

// proseSegment is one slice of a message: either prose (editable) or a fenced
// region (verbatim). Unlike splitUnits, segmentation here PRESERVES the exact
// separators, so joining the segments always reproduces the original bytes when
// nothing is rewritten. That property is what makes a lossy engine safe to run
// on multi-line prompts — rtk and lite can rebuild with explicit "\n" joins
// because they are line-oriented, but caveman edits prose in place and must not
// silently drop newlines.
type proseSegment struct {
	text   string
	inside bool // true = fenced region, never rewritten
}

// segmentProse splits content into prose and fenced segments, keeping every
// byte accounted for. ok=false on an unclosed fence (contract rule 3: skip the
// whole content rather than guess where it ended).
func segmentProse(content string) ([]proseSegment, bool) {
	var segs []proseSegment
	var prose strings.Builder
	inFence := false
	var fence strings.Builder
	fenceLines := 0

	flushProse := func() {
		if prose.Len() > 0 {
			segs = append(segs, proseSegment{text: prose.String()})
			prose.Reset()
		}
	}

	lines := strings.Split(content, "\n")
	for i, ln := range lines {
		last := i == len(lines)-1
		// The separator that follows this line, if any.
		sep := "\n"
		if last {
			sep = ""
		}
		trimmed := strings.TrimSpace(ln)
		isMarker := strings.HasPrefix(trimmed, "```")

		if inFence {
			fence.WriteString(ln)
			fence.WriteString(sep)
			fenceLines++
			if isMarker {
				// Fence closes on this marker line.
				flushProse()
				segs = append(segs, proseSegment{text: fence.String(), inside: true})
				fence.Reset()
				fenceLines = 0
				inFence = false
			}
			continue
		}
		if isMarker {
			// A fence opens: any pending prose becomes its own segment first so
			// the fence stays a contiguous verbatim block.
			flushProse()
			inFence = true
			fence.WriteString(ln)
			fence.WriteString(sep)
			fenceLines++
			continue
		}
		prose.WriteString(ln)
		prose.WriteString(sep)
	}
	if inFence {
		return nil, false // unclosed fence
	}
	flushProse()
	return segs, true
}

// condenseProse runs the protection -> rules -> gates pipeline over one string.
// ok=false means "keep the original" — every failure mode lands there.
func condenseProse(content string, packs []cavemanPack, intensity int) (string, int, bool) {
	trimmed := strings.TrimSpace(content)
	// Pure JSON: not prose. Same guard rtk and lite use.
	if json.Valid([]byte(trimmed)) {
		return content, 0, false
	}

	segs, ok := segmentProse(content)
	if !ok {
		return content, 0, false // unclosed fence => skip entirely (rule 3)
	}

	var b strings.Builder
	fired := 0
	for _, sg := range segs {
		if sg.inside {
			// Fenced region: atomic, verbatim. Not prose, not touched.
			b.WriteString(sg.text)
			continue
		}
		// condenseText operates on the segment INCLUDING its trailing newline;
		// keeping the separator inside the segment is what guarantees the
		// reassembly is byte-faithful.
		rewritten, n, ok := condenseText(sg.text, packs, intensity)
		if !ok {
			// This segment failed a gate — keep it as-is and continue. Per-
			// segment granularity means one damaged segment cannot void a whole
			// message's savings, and cannot smuggle damage through either.
			b.WriteString(sg.text)
			continue
		}
		// Emptied-segment guard: a rule that reduces a prose segment carrying
		// actual words to nothing is the damage pattern worth catching (an
		// over-broad match eating a sentence). Whitespace-only segments are
		// allowed to vanish — that is just cleanup.
		if countWords(sg.text) > 0 && countWords(rewritten) == 0 {
			b.WriteString(sg.text)
			continue
		}
		b.WriteString(rewritten)
		fired += n
	}

	result := b.String()
	if result == content {
		return content, fired, false
	}
	// Gate: no strict byte reduction => not worth the risk.
	if len(result) >= len(content) {
		return content, fired, false
	}
	return result, fired, true
}

// condenseText protects spans, applies the packs, then enforces the
// span-integrity gate. ok=false ⇒ caller keeps the original text.
func condenseText(text string, packs []cavemanPack, intensity int) (string, int, bool) {
	if strings.TrimSpace(text) == "" {
		return text, 0, false
	}
	masked, spans := protect(text)
	rewritten, fired, _ := applyPacks(packs, masked, intensity)
	if fired == 0 || rewritten == masked {
		return text, 0, false
	}
	restored, ok := restore(rewritten, spans)
	if !ok {
		return text, fired, false // placeholder damaged ⇒ discard
	}
	// Span-integrity gate: every protected span must survive, in order.
	if !spansIntact(text, restored, spans) {
		return text, fired, false
	}
	return restored, fired, true
}

// spansIntact verifies that each protected span still appears in the rewritten
// text, in the same left-to-right order. This is the check that makes a lossy
// engine safe to run unattended: a rule cannot delete a path, a URL, a quoted
// string or an identifier without tripping it.
//
// It scans the ORIGINAL (not the masked text) so it catches a rule that somehow
// reproduced a span's content in a different place too — order is what matters.
func spansIntact(original, rewritten string, spans []string) bool {
	pos := 0
	for _, sp := range spans {
		if sp == "" {
			continue
		}
		idx := strings.Index(rewritten[pos:], sp)
		if idx < 0 {
			return false // span vanished
		}
		pos += idx + len(sp)
	}
	// Sanity: the rewritten text must not have grown spans out of nowhere.
	if len(rewritten) > len(original) {
		return false
	}
	return true
}

// intOptionOrNil reads an int option, reporting presence separately from value
// so a caller can distinguish "unset" from "explicitly 0".
func intOptionOrNil(opts map[string]any, key string) (int, bool) {
	if opts == nil {
		return 0, false
	}
	raw, ok := opts[key]
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}
