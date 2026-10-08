package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/compress"
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

func (s *Server) applyCompression(r *http.Request, requested string, body []byte) ([]byte, compress.Result) {
	prof, skip := s.compressionFor(r, requested)
	if prof == nil {
		return body, compress.Result{Profile: profileName(nil, skip)}
	}
	out, res := compress.Apply(body, compressProfileToRuntime(prof))
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
	call.TokensSaved = res.Saved
	call.CompressionMs = &res.MS
	// caveman attribution: non-zero only when prose rules actually rewrote
	// something, so "why did this prompt shrink?" is answerable from the row.
	call.CompressionRulesFired = res.RulesFired
	call.ContextTokensPre = res.ContextPre
	call.ContextTokensSaved = res.ContextSaved
}

// compressionProfileOf is the ledger stamp for rows that never reached
// applyCompression (an early error). It re-derives the skip reason so an
// off/unknown-profile request still records WHY nothing ran.
func (s *Server) compressionProfileOf(r *http.Request, requested string) string {
	prof, skip := s.compressionFor(r, requested)
	return profileName(prof, skip)
}
