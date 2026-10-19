package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/compress"
	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// ── compression at request time (docs/compression-design.md) ────────────────
//
// Resolution chain (M5 slice): x-ezllm-compression header → the combo's
// compression_profile_id → off. Every lookup fails open: unknown name,
// disabled profile, missing combo — the request just proceeds uncompressed.

// compressionFor resolves the profile for a request. The second return is a
// short reason when NOTHING was applied ("off", "unknown-profile",
// "disabled") — the feed shows it so a request that was SUPPOSED to be
// compressed but silently wasn't is visible, not guessable.
func (s *Server) compressionFor(r *http.Request, requested string) (*store.CompressionProfile, string) {
	ctx := r.Context()
	if h := strings.TrimSpace(r.Header.Get("x-ezllm-compression")); h != "" {
		if strings.EqualFold(h, "off") {
			return nil, "off"
		}
		p, err := s.db.CompressionProfileByName(ctx, h)
		if err != nil {
			return nil, "unknown-profile"
		}
		if !p.Enabled {
			return nil, "disabled"
		}
		return p, ""
	}
	// Direct routes are "namespace/model"; combos are the bare-name shape.
	if strings.Contains(requested, "/") {
		return nil, ""
	}
	c, err := s.db.ComboByName(ctx, requested)
	if err != nil || c == nil || c.CompressionProfileID == nil || *c.CompressionProfileID == 0 {
		return nil, ""
	}
	p, err := s.db.GetCompressionProfile(ctx, *c.CompressionProfileID)
	if err != nil {
		return nil, "unknown-profile"
	}
	if !p.Enabled {
		return nil, "disabled"
	}
	return p, ""
}

// compressProfileToRuntime converts a validated store row into the engine's
// runtime view. Stages were validated at save time; a decode failure here
// means a hand-edited row, so the pipeline degrades to a no-op (fail-open).
func compressProfileToRuntime(p *store.CompressionProfile) *compress.Profile {
	var stages []compress.Stage
	if p.Stages != nil {
		if err := json.Unmarshal(p.Stages, &stages); err != nil {
			stages = nil
		}
	}
	return &compress.Profile{
		Name:              p.Name,
		Enabled:           p.Enabled,
		Stages:            stages,
		ExemptLastTurn:    p.ExemptLastTurn,
		MinCompressRatio:  p.MinCompressRatio,
		FailOpen:          p.FailOpen,
		AutoTriggerTokens: p.AutoTriggerTokens,
		FreshToolOutput:   p.FreshToolOutput == "compress",
	}
}

// applyCompression runs the resolved profile over the request body exactly
// once, before candidate dispatch (hops only swap `model`, so every hop
// sends the same compressed body). Returns the body to forward plus the
// ledger truth (contract rule 6).
// profileName is stamped into the ledger (and the feed) whenever a profile
// was RESOLVED — even when nothing compressed — so an explicit
// x-ezllm-compression: off is still distinguishable from "never asked".
func profileName(prof *store.CompressionProfile, skipReason string) string {
	if prof != nil {
		return prof.Name
	}
	return skipReason
}

func (s *Server) applyCompression(r *http.Request, requested string, body []byte, doc *jsonbody.BodyDoc) ([]byte, compress.Result) {
	prof, skip := s.compressionFor(r, requested)
	if prof == nil {
		return body, compress.Result{Profile: profileName(nil, skip)}
	}
	rt := compressProfileToRuntime(prof)
	rt.MinAbsoluteTokens = s.compressionMinAbs
	rt.MaxPipelineMS = s.compressionMaxMS
	// Sampled shadow pass (addendum #7): the sampler decides per request;
	// nil = never. The shadow measurement is discarded, never shipped.
	if s.shadowSampler != nil {
		rt.Shadow = s.shadowSampler()
	}
	// Every request gets committed-prefix stability: an explicit session id
	// wins, otherwise the conversation's own opening derives one. Used only
	// for the memo — the ledger's session_id column stays the explicit id.
	session := sessionOf(r)
	if session == "" && doc != nil && doc.Err() == nil && doc.Map() != nil {
		session = compress.ConversationKey(clientName(r), doc.Map())
	}
	out, res := compress.ApplyWithSession(body, rt, doc, session, s.sessMemo)
	return out, res
}

// attachCompressionMetrics stamps the ledger row with the truth — including
// skips: profile resolved but applied=0 with prompt_tokens_pre recorded is
// exactly what makes a saving auditable.
func attachCompressionMetrics(call *store.Call, res compress.Result) {
	if res.Profile == "" {
		return
	}
	call.CompressionProfile = res.Profile
	call.CompressionApplied = res.Applied
	call.PromptTokensPre = res.Pre
	// Real vs notional: what actually shipped shorter goes in TokensSaved (the
	// dashboard headline); a floor-rejected-but-measurable saving goes in
	// TokensSavedNotional so it is visible without inflating the headline.
	call.TokensSaved = res.Saved
	call.TokensSavedNotional = res.SavedNotional
	// The immutable ruleset behind the decision (addendum #5): the row can be
	// re-derived against the exact profile version that produced it.
	call.CompressionVersion = res.ProfileVersion
	// Per-engine attribution (addendum #7): every stage that ran, with its
	// before/after, as JSON. Empty when nothing ran — an empty column, never
	// a fabricated "[]".
	if len(res.Stages) > 0 {
		if b, err := json.Marshal(res.Stages); err == nil {
			call.CompressionStages = string(b)
		}
	}
	// The sampled POTENTIAL reduction (addendum #7): tuning data that never
	// contributed to what shipped — it lives in its own column so it can
	// never be mistaken for savings.
	call.ShadowTokensSaved = res.ShadowSaved
	call.CompressionMs = &res.MS
	// caveman attribution: non-zero only when prose rules actually rewrote
	// something, so "why did this prompt shrink?" is answerable from the row.
	call.CompressionRulesFired = res.RulesFired
}

// compressionProfileOf is the ledger stamp for rows that never reached
// applyCompression (an early error). It re-derives the skip reason so an
// off/unknown-profile request still records WHY nothing ran.
func (s *Server) compressionProfileOf(r *http.Request, requested string) string {
	prof, skip := s.compressionFor(r, requested)
	return profileName(prof, skip)
}
