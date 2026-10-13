package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/compress"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// ── compression profiles: admin CRUD ────────────────────────────────────────
//
// GET (list) shares handleProfiles; POST and the {id} routes live here.
// Validation happens BEFORE persistence: an unknown engine in a saved
// pipeline would fail open at request time forever, silently — better to
// reject it at the door (docs/compression-design.md).

// profilePayload is a partial-update DTO: nil means "not provided", so PATCH
// overlays exactly what the client sent and POST falls back to schema
// defaults for what it didn't.
type profilePayload struct {
	Name              *string          `json:"name"`
	Enabled           *bool            `json:"enabled"`
	Stages            *json.RawMessage `json:"stages"`
	ExemptLastTurn    *bool            `json:"exempt_last_turn"`
	MinCompressRatio  *float64         `json:"min_compress_ratio"`
	FailOpen          *bool            `json:"fail_open"`
	AutoTriggerTokens *int64           `json:"auto_trigger_tokens"`
	Notes             *string          `json:"notes"`
}

// validateStages parses the pipeline and rejects anything the M5 registry
// cannot run. Returns the canonical JSON (options normalized by re-marshal).
func validateStages(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage("[]")
	}
	var stages []compress.Stage
	if err := json.Unmarshal(raw, &stages); err != nil {
		return nil, err
	}
	if len(stages) == 0 {
		return nil, errStr("stages must contain at least one engine")
	}
	for i, st := range stages {
		if !compress.KnownEngines[st.Engine] {
			return nil, errStrf("stage %d: unknown engine %q (supported: %s)", i, st.Engine, supportedEngines())
		}
		if err := validateStageOptions(i, st); err != nil {
			return nil, err
		}
	}
	return json.Marshal(stages)
}

// supportedEngines renders the registry keys in a STABLE order so this error
// can never drift from KnownEngines. It previously hardcoded a 5-engine list
// that was missing `budget` — the symptom of building the string by hand.
func supportedEngines() string {
	names := make([]string, 0, len(compress.KnownEngines))
	for n := range compress.KnownEngines {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// cavemanIntensities are the ONLY values the caveman engine honours. omniroute's
// aggressive/ultra are deliberately refused: they are pipeline COMPOSITIONS here
// (session_dedup + rtk + headroom + caveman), not prose levels, and accepting
// them silently would let a profile author believe a stronger rewrite was running
// when `standard` was (docs/compression-design.md, "Intensity").
var cavemanIntensities = map[string]bool{"lite": true, "standard": true}

func validateStageOptions(i int, st compress.Stage) error {
	if st.Engine != "caveman" {
		return nil
	}
	raw, ok := st.Options["intensity"]
	if !ok {
		return nil // absent -> engine default (lite)
	}
	v, _ := raw.(string)
	if strings.TrimSpace(v) == "" {
		return nil // explicit "" -> engine treats as lite
	}
	if !cavemanIntensities[strings.ToLower(strings.TrimSpace(v))] {
		return errStrf("stage %d: caveman intensity %q is not a prose level — caveman supports only "+
			"\"lite\"/\"standard\". For omniroute-style aggressive/ultra, compose the pipeline instead: "+
			"session_dedup -> rtk -> headroom -> caveman(standard)", i, v)
	}
	return nil
}

type errStr string

func (e errStr) Error() string { return string(e) }

func errStrf(format string, a ...any) error {
	return errStr(fmt.Sprintf(format, a...))
}

// handleProfile is GET/PATCH/DELETE /admin/compression-profiles/{id}.
func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	switch r.Method {
	case http.MethodGet:
		p, err := s.db.GetCompressionProfile(ctx, id)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)

	case http.MethodPatch:
		existing, err := s.db.GetCompressionProfile(ctx, id)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		var pay profilePayload
		if !decodeBody(w, r, &pay) {
			return
		}
		if pay.Name != nil {
			existing.Name = *pay.Name
		}
		if pay.Enabled != nil {
			existing.Enabled = *pay.Enabled
		}
		if pay.Stages != nil {
			canonical, err := validateStages(*pay.Stages)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			existing.Stages = canonical
		}
		if pay.ExemptLastTurn != nil {
			existing.ExemptLastTurn = *pay.ExemptLastTurn
		}
		if pay.MinCompressRatio != nil {
			existing.MinCompressRatio = *pay.MinCompressRatio
		}
		if pay.FailOpen != nil {
			existing.FailOpen = *pay.FailOpen
		}
		if pay.AutoTriggerTokens != nil {
			existing.AutoTriggerTokens = *pay.AutoTriggerTokens
		}
		if pay.Notes != nil {
			existing.Notes = *pay.Notes
		}
		if err := checkProfileBounds(*existing); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if _, err := s.db.UpsertCompressionProfile(ctx, *existing); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, existing)

	case http.MethodDelete:
		if err := s.db.DeleteCompressionProfile(ctx, id); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})

	default:
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

// handleProfileCreate is POST /admin/compression-profiles.
func (s *Server) handleProfileCreate(w http.ResponseWriter, r *http.Request) {
	var pay profilePayload
	if !decodeBody(w, r, &pay) {
		return
	}
	// Schema defaults (compression_profiles DDL) for anything omitted.
	p := store.CompressionProfile{
		Enabled:          true,
		ExemptLastTurn:   true,
		MinCompressRatio: 0.05,
		FailOpen:         true,
		Stages:           json.RawMessage("[]"),
	}
	if pay.Name != nil {
		p.Name = *pay.Name
	}
	if pay.Enabled != nil {
		p.Enabled = *pay.Enabled
	}
	if pay.Stages != nil {
		p.Stages = *pay.Stages
	}
	if pay.ExemptLastTurn != nil {
		p.ExemptLastTurn = *pay.ExemptLastTurn
	}
	if pay.MinCompressRatio != nil {
		p.MinCompressRatio = *pay.MinCompressRatio
	}
	if pay.FailOpen != nil {
		p.FailOpen = *pay.FailOpen
	}
	if pay.AutoTriggerTokens != nil {
		p.AutoTriggerTokens = *pay.AutoTriggerTokens
	}
	if pay.Notes != nil {
		p.Notes = *pay.Notes
	}
	if p.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "name is required")
		return
	}
	canonical, err := validateStages(p.Stages)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	p.Stages = canonical
	if err := checkProfileBounds(p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	id, err := s.db.UpsertCompressionProfile(r.Context(), p)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	p.ID = id
	writeJSON(w, http.StatusCreated, p)
}

func checkProfileBounds(p store.CompressionProfile) error {
	if p.MinCompressRatio < 0 || p.MinCompressRatio >= 1 {
		return errStr("min_compress_ratio must be in [0,1)")
	}
	if p.AutoTriggerTokens < 0 {
		return errStr("auto_trigger_tokens must be >= 0")
	}
	return nil
}
