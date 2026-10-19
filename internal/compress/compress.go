// Package compress implements ezllm's compression contract (rev4 plan §4.2,
// docs/compression-design.md): deterministic, pure-Go prompt compression.
// It is LOSSY and cannot help unconditionally — the contract's job is to
// make that visible: every skip path returns the ORIGINAL body bytes
// (fail-open means byte-identical passthrough, never a re-marshal), only a
// shipped rewrite counts as Applied savings, and a rejected rewrite books
// its would-have-saved into Notional. What the engines discard is gone —
// see docs/compression-design.md and the per-engine scope notes.
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

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
)

// Stage is one entry of a profile's ordered pipeline:
// {"engine":"session_dedup"} or {"engine":"rtk","keep_lines":6}.
type Stage struct {
	Engine  string         `json:"engine"`
	Options map[string]any `json:"options,omitempty"`
}

// KnownEngines is the shipped engine registry (M5: session_dedup + rtk,
// M6.5 phase 2: headroom + lite, M7: caveman).
var KnownEngines = map[string]bool{"session_dedup": true, "rtk": true, "headroom": true, "lite": true, "caveman": true}

// Profile is the runtime view of a compression_profiles row.
type Profile struct {
	Name              string
	Enabled           bool
	Stages            []Stage
	ExemptLastTurn    bool
	MinCompressRatio  float64
	FailOpen          bool
	AutoTriggerTokens int64
	// MinAbsoluteTokens (addendum #5): the new work must remove at least
	// this many estimated tokens before it ships. A ratio alone lets a
	// 3-token saving through on a big prompt — real bytes, real cache churn,
	// no measurable benefit. 0 = no absolute floor.
	MinAbsoluteTokens int64
	// MaxPipelineMS (addendum #5): the CPU limit for the stage loop. When a
	// stage boundary passes it, the pipeline stops and ships what already
	// ran — compression must never be the most expensive part of a request.
	// 0 = no limit.
	MaxPipelineMS int64
	// Shadow (addendum #7): run a sampled shadow pass after the real one —
	// the engines over EVERYTHING with the floors ignored, discarded, and
	// measured as potential. Armed per request by the sampler; the shadow
	// transform never ships and never commits.
	Shadow bool
}

// StageStat is one stage's measured contribution (addendum #5/#7 per-engine
// attribution). Sequential before/after: Pre is what entered the stage, Post
// what left it, Saved the difference.
//
// RECONCILIATION RULE (documented, not assumed): the stage stats measure the
// EDITABLE SLICE's pipeline; the ledger's net reduction measures the WHOLE
// REQUEST (slice spliced back into the body). The two agree up to boundary
// effects — the slice's standalone marshal differs from its marshal in place
// by a token at most, so SUM(Saved) may differ from the net by 1-2 tokens.
// Both totals ride the row (compression_stages vs tokens_saved), so a larger
// discrepancy is VISIBLE rather than silently absorbed. Every stage is
// reductive by contract (an engine that would expand keeps its input), so
// the sum never exceeds the net.
type StageStat struct {
	Engine string `json:"engine"`
	Pre    int64  `json:"pre"`
	Post   int64  `json:"post"`
	Saved  int64  `json:"saved"`
	Fired  int    `json:"fired"`
	CPUms  int64  `json:"cpu_ms"`
}

// Result carries the truth for the ledger (contract rule 6).
// Profile is set whenever a profile RESOLVED (even if engines then skipped);
// Applied is true only when the rewritten body is what ships upstream.
type Result struct {
	Profile string
	// ProfileVersion is the immutable fingerprint of the effective profile
	// that made THIS decision (addendum #5: every accepted transformation is
	// attributable to the exact ruleset that produced it — the same binding
	// the session memo uses, so a ledger row can be re-derived).
	ProfileVersion string
	Applied        bool
	Pre            int64 // estimated prompt tokens before compression
	// Saved is what ACTUALLY shipped shorter — 0 on every skip/fail path, so
	// SUM(Saved) is a real saving, not an aspiration.
	Saved int64 // estimated tokens saved (0 on any skip)
	// SavedNotional is what compression WOULD have saved had the ratio floor /
	// integrity gate let the rewrite through. Recorded so a rejected-but-large
	// saving is visible for tuning without polluting Saved.
	SavedNotional int64
	MS            int64
	// RulesFired is caveman attribution: how many pack rules rewrote
	// something. Zero for every other engine. Recorded so a surprising
	// saving can be traced to prose condensation rather than guessed at.
	RulesFired int
	// Stages is the per-engine attribution (addendum #7): every stage that
	// RAN, in order, with its before/after. It reconciles with Saved via the
	// documented rule on StageStat.
	Stages []StageStat
	// ShadowSaved is the POTENTIAL reduction a sampled shadow run measured
	// (addendum #7): the engines over everything, with the floors ignored,
	// for tuning. It NEVER contributes to applied savings — the shadow
	// transform is discarded, not shipped.
	ShadowSaved int64
	Err         string // diagnostics for tests/logs; never fails the request
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
	return ApplyWithDoc(body, p, nil)
}

// ApplyWithDoc is Apply with the request's single-parse decode threaded
// through (docs/single-parse-dispatch-design.md): when doc is non-nil its
// map(root) replaces the internal decode — same UseNumber settings, same
// fail-open decisions — so the body is not parsed a second time. doc must be
// the decode of `body`; a nil doc decodes internally as before.
//
// Results are NAMED (out, res): the deferred MS stamp below only reaches
// the caller on named returns — with unnamed returns the deferred write
// lands on a local copy and compression_ms silently reads 0 everywhere.
//
// This is the LEGACY stateless entry: no session, no memo. Request-path
// callers should use ApplyWithSession, which adds committed-prefix
// stability on top of exactly this pipeline.
func ApplyWithDoc(body []byte, p *Profile, doc *jsonbody.BodyDoc) ([]byte, Result) {
	return applyPipeline(body, p, doc, "", nil)
}

// ApplyWithSession is ApplyWithDoc with committed-prefix stability (WS-C,
// scoped): when session and memo are both non-empty, messages already
// SHIPPED in a transformed form (this session, this profile) ship that
// exact form again — never re-judged by the ratio floor — and the floor
// applies to the new work only. With an empty session or nil memo the
// behaviour is exactly ApplyWithDoc's (legacy stateless mode). See
// sessionmemo.go for the ledger's bounds, TTL, and privacy properties.
func ApplyWithSession(body []byte, p *Profile, doc *jsonbody.BodyDoc, session string, memo *SessionMemo) ([]byte, Result) {
	return applyPipeline(body, p, doc, session, memo)
}

// applyPipeline is the shared pipeline; session/memo empty = legacy mode.
func applyPipeline(body []byte, p *Profile, doc *jsonbody.BodyDoc, session string, memo *SessionMemo) (out []byte, res Result) {
	start := time.Now()
	// A disabled/stageless profile still RESOLVED, so its name belongs on
	// the row; a nil profile has no name (and must never be dereferenced).
	if p != nil {
		res.Profile = p.Name
	}
	// The ruleset identity that decides this request (addendum #5). Stamped
	// for EVERY profile, including skips, so any ledger row — applied,
	// declined, or never-transformed — names the exact version that looked
	// at it. A session's BOUND version overrides below when the memo is on.
	res.ProfileVersion = ProfileFingerprint(p)
	defer func() { res.MS = time.Since(start).Milliseconds() }()

	if p == nil || !p.Enabled || len(p.Stages) == 0 {
		return body, res
	}

	// Chat-completions shape only (M5): anything without a messages array
	// (/v1/responses input shape, anthropic blocks at top level, ...) passes
	// through untouched with the profile truth recorded.
	// Single-parse: the request's shared decode is the root map (read-only
	// here — engines work on the messages slice copy), so this is where the
	// per-request body parse disappears when doc is provided. Identical
	// semantics to the decode below: UseNumber + non-object refusal.
	var root map[string]any
	if doc != nil {
		if doc.Err() != nil {
			res.Err = "body is not a JSON object"
			return body, res
		}
		root = doc.Map()
		if root == nil {
			res.Err = "body is not a JSON object"
			return body, res
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&root); err != nil || root == nil {
			res.Err = "body is not a JSON object"
			return body, res
		}
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

	// WS-C committed-prefix stability: messages already SHIPPED in a
	// transformed form (this session, this profile) are replayed verbatim
	// and excluded from the new work. The ratio floor then judges new work
	// only, so accepted history can never flip back to original bytes as the
	// conversation grows, and a message that shipped untouched stays
	// untouched when the exempt window slides off it. Order is preserved:
	// hit[] marks the replayed positions, cur carries the new work in order.
	// The session binds ONCE to the effective profile's fingerprint
	// (addendum #4): an operator editing the profile never silently
	// re-versions a live session — its committed bytes keep replaying under
	// the version it was first seen with, and only new sessions bind fresh.
	profileVersion := memo.Bind(session, res.ProfileVersion)
	if profileVersion != "" {
		// The DECISION ran under the bound version (its committed forms are
		// from it), so that is what the row must name.
		res.ProfileVersion = profileVersion
	}
	useMemo := profileVersion != ""
	hit := make([]bool, len(editable))
	forms := make([]any, len(editable)) // committed forms for hit positions
	newWork := make([]any, 0, len(editable))
	if useMemo {
		for i, m := range editable {
			if form, ok := memo.Get(session, profileVersion, m); ok {
				hit[i] = true
				forms[i] = form
				continue
			}
			newWork = append(newWork, m)
		}
	} else {
		newWork = append(newWork, editable...)
	}

	cur := newWork
	stagesRan := 0
	for _, st := range p.Stages {
		if !KnownEngines[st.Engine] {
			res.Err = "unknown engine " + st.Engine
			return body, res // fail-open
		}
		// CPU limit (addendum #5): the pipeline stops at a stage boundary once
		// it has spent its budget. What already ran ships (a deterministic,
		// attributable partial pipeline); what did not run does not.
		if p.MaxPipelineMS > 0 && stagesRan > 0 && time.Since(start).Milliseconds() > p.MaxPipelineMS {
			res.Err = fmt.Sprintf("cpu cap: stopped after %d/%d stages at %dms", stagesRan, len(p.Stages), time.Since(start).Milliseconds())
			break
		}
		stageStart := time.Now()
		beforeStage := EstimateTokens(string(mustMarshal(cur)))
		next, fired, err := runEngine(st, cur)
		if err != nil {
			res.Err = st.Engine + ": " + err.Error()
			return body, res // contract rule 5
		}
		postStage := EstimateTokens(string(mustMarshal(next)))
		st := StageStat{Engine: st.Engine, Pre: beforeStage, Post: postStage, Fired: fired}
		if beforeStage > postStage {
			st.Saved = beforeStage - postStage
		}
		st.CPUms = time.Since(stageStart).Milliseconds()
		res.Stages = append(res.Stages, st)
		res.RulesFired += fired
		cur = next
		stagesRan++
	}

	// Shadow pass (addendum #7): sampled, DISCARDED, and measured. The
	// engines run over the whole editable set with the floors ignored and
	// nothing from it ships or commits — the number exists so the gates can
	// be tuned with evidence instead of guesses. Bounded by the same CPU
	// policy: a pipeline that already spent its budget skips the shadow.
	if p.Shadow && len(editable) > 0 && (p.MaxPipelineMS == 0 || time.Since(start).Milliseconds() <= p.MaxPipelineMS) {
		shadowPre := EstimateTokens(string(mustMarshal(editable)))
		shadow := editable
		for _, st := range p.Stages {
			next, _, err := runEngine(st, shadow)
			if err != nil {
				break
			}
			shadow = next
		}
		shadowPost := EstimateTokens(string(mustMarshal(shadow)))
		if shadowPre > shadowPost {
			res.ShadowSaved = shadowPre - shadowPost
		}
	}

	// Integrity (contract rule 3): every modified text must still be
	// structurally sound — balanced fences. JSON integrity holds by
	// construction (rtk skips JSON whole; marshal can't produce imbalance).
	if msg := integrityFailure(cur); msg != "" {
		res.Err = "integrity: " + msg
		return body, res
	}

	// Contract rule 4 applied to the NEW WORK ONLY: the floor judges this
	// request's fresh rewrites. A rejection discards them (they ship
	// original) while committed history rides along unchanged — re-judging
	// committed content is exactly the flip-flop this design removes.
	floorNote := ""
	if len(newWork) > 0 {
		newPre := EstimateTokens(string(mustMarshal(newWork)))
		newPost := EstimateTokens(string(mustMarshal(cur)))
		wouldSave := newPre - newPost
		// Ratio floor first (cheapest check), then the ABSOLUTE floor
		// (addendum #5): a ratio that passes on a tiny saving is still not
		// worth the bytes. Both judge the new work only.
		if newPre > 0 && float64(wouldSave)/float64(newPre) < p.MinCompressRatio {
			res.SavedNotional = wouldSave
			floorNote = fmt.Sprintf("ratio floor (new work): %.3f < %.3f", float64(wouldSave)/float64(newPre), p.MinCompressRatio)
			cur = newWork // the new work ships original
		} else if wouldSave > 0 && p.MinAbsoluteTokens > 0 && wouldSave < p.MinAbsoluteTokens {
			res.SavedNotional = wouldSave
			floorNote = fmt.Sprintf("absolute floor (new work): %d < %d tokens", wouldSave, p.MinAbsoluteTokens)
			cur = newWork
		}
	}

	// Reassemble. Positional mapping (exact original order) is used when the
	// pipeline preserved the message count. A dropping engine (session_dedup
	// removing a repeated system message) shortens the slice, so exact
	// positions are no longer known: committed forms keep their relative
	// order first, then the survivors of the new work, then the exempt tail.
	var rebuilt []any
	if len(cur) == len(newWork) {
		rebuilt = make([]any, 0, len(editable))
		ni := 0
		for i := range editable {
			if hit[i] {
				rebuilt = append(rebuilt, forms[i])
				continue
			}
			rebuilt = append(rebuilt, cur[ni])
			ni++
		}
	} else {
		rebuilt = make([]any, 0, len(editable)+len(cur))
		for _, f := range forms {
			if f != nil {
				rebuilt = append(rebuilt, f)
			}
		}
		rebuilt = append(rebuilt, cur...)
	}
	finalMsgs := rebuilt
	if p.ExemptLastTurn && last > 0 {
		finalMsgs = append(append([]any{}, rebuilt...), msgs[last])
	}

	post := EstimateTokens(string(mustMarshal(finalMsgs)))
	saved := pre - post
	if saved <= 0 {
		saved = 0
	}
	if saved == 0 {
		res.Err = floorNote
		if res.Err == "" {
			res.Err = "nothing saved"
		}
		// The engines RAN and this body is what ships: freeze it verbatim so
		// the same bytes go upstream next request (the exempt tail commit
		// lives here too — it went upstream untouched).
		commitShipped(session, memo, profileVersion, p, editable, hit, forms, cur, newWork, last, msgs)
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
	res.Saved = saved
	res.Applied = true
	if floorNote != "" {
		res.Err = floorNote
	}

	// Commit what actually SHIPPED — every editable message and the exempt
	// tail, in the form it went upstream (transformed OR verbatim). Freezing
	// verbatim forms is deliberate: those bytes already went upstream, so
	// rewriting them on a later request would break the very prefix this
	// design protects. The two-directional crossing therefore happens only
	// through an EXPLICIT transition (a profile change starts a fresh
	// namespace), never through silent re-compression.
	commitShipped(session, memo, profileVersion, p, editable, hit, forms, cur, newWork, last, msgs)
	return out, res
}

// commitShipped freezes the shipped form of every processed message. It is a
// no-op in legacy mode (no session/memo) and when a dropping engine ran
// (positions are no longer aligned).
func commitShipped(session string, memo *SessionMemo, profileVersion string, p *Profile, editable []any, hit []bool, forms []any, cur []any, newWork []any, last int, msgs []any) {
	if profileVersion == "" || len(cur) != len(newWork) {
		return
	}
	ni := 0
	for i, m := range editable {
		if !hit[i] {
			memo.Put(session, profileVersion, m, cur[ni])
			ni++
		}
	}
	if p.ExemptLastTurn && last > 0 {
		memo.Put(session, profileVersion, msgs[last], msgs[last])
	}
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

// isJSONPayload reports whether text is a whole JSON object or array. Line
// filters must never edit such content (contract rule 2, "never mid-JSON"),
// whichever wire shape carried it.
func isJSONPayload(text string) bool {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return false
	}
	return json.Valid([]byte(trimmed))
}

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
