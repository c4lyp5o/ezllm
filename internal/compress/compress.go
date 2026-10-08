// Package compress implements ezllm's compression contract (rev4 plan §4.2,
// docs/compression-design.md): deterministic, pure-Go prompt compression that
// can only ever help. Every skip path returns the ORIGINAL body bytes —
// fail-open means byte-identical passthrough, never a re-marshal.
package compress

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Stage is one entry of a profile's ordered pipeline:
// {"engine":"session_dedup"} or {"engine":"rtk","keep_lines":6}.
type Stage struct {
	Engine  string         `json:"engine"`
	Options map[string]any `json:"options,omitempty"`
}

// KnownEngines is the shipped engine registry (M5: session_dedup + rtk,
// M6.5 phase 2: headroom + lite, M7: caveman).
var KnownEngines = map[string]bool{"session_dedup": true, "rtk": true, "headroom": true, "lite": true, "caveman": true, "budget": true}

// Profile is the runtime view of a compression_profiles row.
type Profile struct {
	Name              string
	Enabled           bool
	Stages            []Stage
	ExemptLastTurn    bool
	MinCompressRatio  float64
	FailOpen          bool
	AutoTriggerTokens int64
}

// Result carries the truth for the ledger (contract rule 6).
// Profile is set whenever a profile RESOLVED (even if engines then skipped);
// Applied is true only when the rewritten body is what ships upstream.
type Result struct {
	Profile string
	Applied bool
	Pre     int64 // estimated prompt tokens before compression
	Saved   int64 // estimated tokens saved (0 on any skip)
	MS      int64
	// RulesFired is caveman attribution: how many pack rules rewrote
	// something. Zero for every other engine. Recorded so a surprising
	// saving can be traced to prose condensation rather than guessed at.
	RulesFired   int
	ContextPre   int64
	ContextSaved int64
	Err          string // diagnostics for tests/logs; never fails the request
}

// EstimateTokens is the shared pure-Go estimator (no tokenizer dependency:
// the no-CGO/static image constraint). Same estimator feeds the floor
// decision and the ledger, so ratios are internally consistent.
func EstimateTokens(s string) int64 {
	return int64(utf8.RuneCountInString(s)/4) + 1
}

// Apply runs the profile's pipeline over a chat-completions body.
// body is returned unchanged on every skip/fail path.
func Apply(body []byte, p *Profile) ([]byte, Result) {
	start := time.Now()
	res := Result{Profile: p.Name}
	defer func() { res.MS = time.Since(start).Milliseconds() }()

	if p == nil || !p.Enabled || len(p.Stages) == 0 {
		return body, res
	}

	// Chat-completions shape only (M5): anything without a messages array
	// (/v1/responses input shape, anthropic blocks at top level, ...) passes
	// through untouched with the profile truth recorded.
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil || root == nil {
		res.Err = "body is not a JSON object"
		return body, res
	}
	rawMsgs, ok := root["messages"].([]any)
	if !ok || len(rawMsgs) == 0 {
		res.Err = "no messages array"
		return body, res
	}

	// Work on a deep copy via round-trip so the original body bytes stay
	// intact for every fail-open path.
	msgs := rawMsgs
	pre := EstimateTokens(string(mustMarshal(msgs)))
	res.Pre = pre

	// auto_trigger_tokens: 0 = always, else only above N.
	if p.AutoTriggerTokens > 0 && pre < p.AutoTriggerTokens {
		res.Err = fmt.Sprintf("below auto_trigger_tokens(%d)", p.AutoTriggerTokens)
		return body, res
	}

	// Contract rule 1: the final message is exempt when the profile says so.
	last := len(msgs) - 1
	editable := msgs
	if p.ExemptLastTurn && last > 0 {
		editable = msgs[:last]
	}

	cur := editable
	for _, st := range p.Stages {
		if !KnownEngines[st.Engine] {
			res.Err = "unknown engine " + st.Engine
			return body, res // fail-open
		}
		beforeStage := EstimateTokens(string(mustMarshal(cur)))
		next, fired, err := runEngine(st, cur)
		if err != nil {
			res.Err = st.Engine + ": " + err.Error()
			return body, res // contract rule 5
		}
		if st.Engine == "budget" {
			res.ContextPre = beforeStage
			postStage := EstimateTokens(string(mustMarshal(next)))
			if beforeStage > postStage {
				res.ContextSaved = beforeStage - postStage
			}
		}
		res.RulesFired += fired
		cur = next
	}

	// Integrity (contract rule 3): every modified text must still be
	// structurally sound — balanced fences. JSON integrity holds by
	// construction (rtk skips JSON whole; marshal can't produce imbalance).
	if msg := integrityFailure(cur); msg != "" {
		res.Err = "integrity: " + msg
		return body, res
	}

	// Reassemble with the exempt final message appended back byte-identical.
	finalMsgs := cur
	if p.ExemptLastTurn && last > 0 {
		finalMsgs = append(append([]any{}, cur...), msgs[last])
	}

	post := EstimateTokens(string(mustMarshal(finalMsgs)))
	saved := pre - post
	if saved <= 0 {
		saved = 0
	}
	res.Saved = saved

	// Contract rule 4: ratio floor — below it, the original ships.
	if pre > 0 && float64(saved)/float64(pre) < p.MinCompressRatio {
		res.Err = fmt.Sprintf("ratio floor: %.3f < %.3f", float64(saved)/float64(pre), p.MinCompressRatio)
		return body, res
	}
	if saved == 0 {
		res.Err = "nothing saved"
		return body, res
	}

	// Ship the rewrite: round-trip the root map (model + every unknown
	// field preserved — same invariant as SwapModel).
	root["messages"] = finalMsgs
	out, err := json.Marshal(root)
	if err != nil {
		res.Err = "re-marshal: " + err.Error()
		return body, res
	}
	res.Applied = true
	return out, res
}

// runEngine applies one stage. fired is only meaningful for caveman (the count
// of rules that actually rewrote something); every other engine reports 0.
func runEngine(st Stage, msgs []any) ([]any, int, error) {
	switch st.Engine {
	case "session_dedup":
		return sessionDedup(msgs), 0, nil
	case "rtk":
		return rtk(msgs, st.Options), 0, nil
	case "headroom":
		return headroom(msgs, st.Options), 0, nil
	case "lite":
		return lite(msgs, st.Options), 0, nil
	case "caveman":
		// caveman also reports how many rules fired, so a surprising saving
		// is attributable to a rule rather than a mystery.
		out, fired := caveman(msgs, st.Options)
		return out, fired, nil
	case "budget":
		return budget(msgs, st.Options), 0, nil
	}
	return nil, 0, fmt.Errorf("unknown engine %q", st.Engine)
}

// integrityFailure returns a non-empty reason if any text inside msgs has an
// unbalanced code fence (rule 3). Empty string = pass.
func integrityFailure(msgs []any) string {
	for i, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			return fmt.Sprintf("message %d is not an object", i)
		}
		for _, s := range textPieces(mm["content"]) {
			if fences(s)%2 != 0 {
				return fmt.Sprintf("message %d has an unclosed code fence", i)
			}
		}
	}
	return ""
}

func fences(s string) int { return strings.Count(s, "```") }

// textPieces flattens a message content (string or text-block array) into its
// text strings. Non-text content yields nothing.
func textPieces(content any) []string {
	switch c := content.(type) {
	case string:
		return []string{c}
	case []any:
		var out []string
		for _, b := range c {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := bm["type"].(string); t == "text" {
				if s, ok := bm["text"].(string); ok {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return nil
}

// hashContent builds the dedup key for droppable content, or "" when the
// content shape is not safe to hash-and-drop.
func hashContent(role string, content any) string {
	var s string
	switch c := content.(type) {
	case string:
		s = c
	case []any:
		// Only arrays of pure {"type":"text"} blocks are droppable.
		var b strings.Builder
		for _, blk := range c {
			bm, ok := blk.(map[string]any)
			if !ok {
				return ""
			}
			if t, _ := bm["type"].(string); t != "text" {
				return ""
			}
			ts, ok := bm["text"].(string)
			if !ok {
				return ""
			}
			b.WriteString(ts)
			b.WriteByte(0)
		}
		s = b.String()
	default:
		return ""
	}
	sum := sha256.Sum256([]byte(role + "\x00" + s))
	return hex.EncodeToString(sum[:])
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Values came from a JSON decode; marshal cannot fail here.
		panic("compress: marshal: " + err.Error())
	}
	return b
}
