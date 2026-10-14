package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// serviceCard is one row of GET /admin/services: everything a Services card
// needs to render honest state without a second fetch. docs/API.md §M10 is the
// contract; api.ts mirrors these tags field-for-field.
type serviceCard struct {
	Surface      string          `json:"surface"`
	Endpoint     string          `json:"endpoint"`
	Label        string          `json:"label"`
	Description  string          `json:"description"`
	Mode         string          `json:"mode"` // "manual" | "auto" | "off"
	ModelTarget  string          `json:"model_target"`
	Resolved     string          `json:"resolved_target"`
	TargetExists bool            `json:"target_exists"`
	Verified     int             `json:"verified_models"`
	Accounts     int             `json:"accounts"`
	Verdict      *serviceVerdict `json:"verdict"`
}

type serviceVerdict struct {
	Model    string `json:"model"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
	TestedAt string `json:"tested_at"`
}

// serviceDefs maps each service surface to its endpoint and first-run copy.
// The list order is the card order.
var serviceDefs = []struct {
	surface, endpoint, label, description string
}{
	{"embeddings", "/v1/embeddings", "Embeddings",
		"Vector embeddings for search, RAG, clustering, and similarity."},
	{"images_gen", "/v1/images/generations", "Image generation",
		"Text-to-image generation for creative and production workloads."},
	{"audio_tts", "/v1/audio/speech", "Text to speech",
		"Synthesize spoken audio from text."},
	{"audio_stt", "/v1/audio/transcriptions", "Speech to text",
		"Transcribe audio into text."},
}

func serviceDef(surface string) (endpoint, label, description string, ok bool) {
	for _, d := range serviceDefs {
		if d.surface == surface {
			return d.endpoint, d.label, d.description, true
		}
	}
	return "", "", "", false
}

// handleServices lists all four service cards — always all four, including
// unbound ones, so a first-time user sees what ezllm can do and exactly what is
// not configured yet.
func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	enabled, err := s.db.CountEnabledAccounts(ctx)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	out := make([]serviceCard, 0, len(serviceDefs))
	bindings, err := s.db.ListServiceBindings(ctx)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	bySurface := map[string]store.ServiceBinding{}
	for _, b := range bindings {
		bySurface[b.Surface] = b
	}
	for _, def := range serviceDefs {
		card := serviceCard{
			Surface:     def.surface,
			Endpoint:    def.endpoint,
			Label:       def.label,
			Description: def.description,
			Mode:        "off",
			Accounts:    enabled,
		}
		if b, found := bySurface[def.surface]; found {
			card.Mode = b.Mode
			card.ModelTarget = b.ModelTarget
		}
		switch card.Mode {
		case "manual":
			card.Resolved = card.ModelTarget
			exists, err := s.serviceTargetExists(ctx, card.ModelTarget, def.surface)
			if err != nil {
				s.writeStoreErr(w, err)
				return
			}
			card.TargetExists = exists
		case "auto":
			target, err := s.db.ResolveServiceDefault(ctx, def.surface)
			if err != nil {
				s.writeStoreErr(w, err)
				return
			}
			card.Resolved = target
			card.TargetExists = target != ""
		}
		probes, err := s.db.ListServiceProbes(ctx, def.surface)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		for _, p := range probes {
			if p.OK {
				card.Verified++
			}
		}
		if card.Resolved != "" {
			v, err := s.probeForTarget(ctx, probes, card.Resolved)
			if err != nil {
				s.writeStoreErr(w, err)
				return
			}
			card.Verdict = v
		}
		out = append(out, card)
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": out})
}

// probeForTarget returns the stored verdict for a direct namespaced model
// target. Combo targets carry no per-model verdict (nil).
func (s *Server) probeForTarget(ctx context.Context, probes []store.ServiceProbe, target string) (*serviceVerdict, error) {
	i := strings.IndexByte(target, '/')
	if i <= 0 {
		return nil, nil
	}
	acct, err := s.db.AccountByNamespace(ctx, target[:i])
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, p := range probes {
		if p.AccountID == acct.ID && p.ModelID == target[i+1:] {
			return &serviceVerdict{Model: target, OK: p.OK, Detail: p.Detail, TestedAt: p.TestedAt}, nil
		}
	}
	return nil, nil
}

type serviceBindingRequest struct {
	Mode  string `json:"mode"`
	Model string `json:"model"`
}

// handleServiceBinding stores the default for one service surface. Strict
// decode; "off" clears the row; "manual" requires a resolvable model; "auto"
// ignores model.
func (s *Server) handleServiceBinding(w http.ResponseWriter, r *http.Request) {
	surface := r.PathValue("surface")
	if _, _, _, ok := serviceDef(surface); !ok {
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("unknown service %q — expected one of embeddings, images_gen, audio_tts, audio_stt", surface))
		return
	}
	var req serviceBindingRequest
	if !decodeBody(w, r, &req) {
		return
	}
	ctx := r.Context()
	switch req.Mode {
	case "off":
		if err := s.db.ClearServiceBinding(ctx, surface); err != nil {
			s.writeStoreErr(w, err)
			return
		}
	case "auto":
		if err := s.db.SetServiceBinding(ctx, surface, "auto", ""); err != nil {
			s.writeStoreErr(w, err)
			return
		}
	case "manual":
		target := strings.TrimSpace(req.Model)
		if target == "" {
			writeErr(w, http.StatusBadRequest, "invalid_request_error",
				"manual service binding requires \"model\"")
			return
		}
		exists, err := s.serviceTargetExists(ctx, target, surface)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		if !exists {
			writeErr(w, http.StatusNotFound, "model_not_found",
				fmt.Sprintf("service target %q does not exist in an enabled provider catalog or enabled combo", target))
			return
		}
		if err := s.db.SetServiceBinding(ctx, surface, "manual", target); err != nil {
			s.writeStoreErr(w, err)
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			"\"mode\" must be one of manual, auto, off")
		return
	}
	writeJSON(w, http.StatusOK, s.serviceCardFor(ctx, surface))
}

// serviceCardFor rebuilds one card after a write so the response is the same
// shape the list endpoint serves (no second round trip needed to refresh).
func (s *Server) serviceCardFor(ctx context.Context, surface string) serviceCard {
	endpoint, label, description, _ := serviceDef(surface)
	card := serviceCard{Surface: surface, Endpoint: endpoint, Label: label,
		Description: description, Mode: "off"}
	if b, err := s.db.ServiceBinding(ctx, surface); err == nil {
		card.Mode = b.Mode
		card.ModelTarget = b.ModelTarget
	} else if !errors.Is(err, store.ErrNotFound) {
		return card
	}
	enabled, err := s.db.CountEnabledAccounts(ctx)
	if err == nil {
		card.Accounts = enabled
	}
	switch card.Mode {
	case "manual":
		card.Resolved = card.ModelTarget
		if exists, err := s.serviceTargetExists(ctx, card.ModelTarget, surface); err == nil {
			card.TargetExists = exists
		}
	case "auto":
		if target, err := s.db.ResolveServiceDefault(ctx, surface); err == nil {
			card.Resolved = target
			card.TargetExists = target != ""
		}
	}
	if probes, err := s.db.ListServiceProbes(ctx, surface); err == nil {
		for _, p := range probes {
			if p.OK {
				card.Verified++
			}
		}
		if card.Resolved != "" {
			if v, err := s.probeForTarget(ctx, probes, card.Resolved); err == nil {
				card.Verdict = v
			}
		}
	}
	return card
}

// handleServiceTest runs one real, endpoint-appropriate request through the
// same dispatcher as production traffic (ResolveCandidates → ForwardCandidates)
// and records the verdict per (surface, account, model). Green means "the wire
// answers this service shape" — not output quality. Image probes cost money;
// this endpoint is only ever called from an explicit UI action.
func (s *Server) handleServiceTest(w http.ResponseWriter, r *http.Request) {
	surface := r.PathValue("surface")
	if _, _, _, ok := serviceDef(surface); !ok {
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("unknown service %q — expected one of embeddings, images_gen, audio_tts, audio_stt", surface))
		return
	}
	var req struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	target := strings.TrimSpace(req.Model)
	if target == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			"\"model\" is required — test an explicit ns/model")
		return
	}
	prompt := strings.TrimSpace(req.Prompt)
	if surface == "audio_stt" && prompt != "" {
		// Never silently drop an input the operator believes was tested.
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			"audio_transcriptions tests a file upload, not text — it always sends a fixed 0.2 s silent WAV; leave \"prompt\" empty")
		return
	}
	if len(prompt) > 2000 {
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			"\"prompt\" must be 2000 bytes or fewer — this is a wire-shape test, not a workload")
		return
	}
	ctx := r.Context()
	svc := provider.Surface(surface)
	routes, err := s.resolver.ResolveCandidates(ctx, target, svc, "service-test")
	if err != nil {
		var nf *router.ErrNotFound
		var nu *router.ErrUnavailable
		switch {
		case errors.As(err, &nf):
			writeErrExtra(w, http.StatusNotFound, "model_not_found", err.Error(),
				map[string]any{"available": nf.Hints})
		case errors.As(err, &nu):
			writeErrExtra(w, http.StatusTooManyRequests, "model_unavailable", err.Error(),
				map[string]any{"reason": nu.Reason})
		default:
			writeErr(w, http.StatusBadGateway, "upstream_error", err.Error())
		}
		return
	}
	if len(routes) == 0 {
		writeErr(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("no route serves %q on %s", target, surface))
		return
	}
	body, contentType := serviceProbeBody(surface, target, prompt)
	reqUp, err := http.NewRequestWithContext(ctx, http.MethodPost, serviceProbePath(surface), bytes.NewReader(body))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if contentType != "" {
		reqUp.Header.Set("Content-Type", contentType)
	}
	start := time.Now()
	// The dispatcher returns the route that ACTUALLY served (its contract:
	// routes[0] would attribute another hop's spend). Attribution follows that
	// return, never the candidate order.
	servedRoute, res, ferr := s.dispatcher.ForwardCandidates(ctx, &captureWriter{}, reqUp, routes, body)
	ms := time.Since(start).Milliseconds()
	if servedRoute.Account.ID == 0 {
		// Nothing left the box (every candidate was skipped or refused before
		// dispatch). This is NOT evidence about the wire: no verdict row, no
		// ledger row — exactly the dispatcher's "a skipped hop writes no
		// ledger row" rule. Report the refusal so the operator knows the test
		// did not happen.
		detail := "test did not reach the provider"
		if ferr != nil {
			detail = "test did not reach the provider: " + ferr.Error()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"surface": surface, "model": target, "ok": false, "dispatched": false,
			"status": 0, "ms": ms, "detail": truncate(detail, 500),
		})
		return
	}
	served := servedRoute.Account.Namespace + "/" + servedRoute.Model
	status := res.Status
	detail := ""
	switch {
	case ferr != nil:
		status = http.StatusBadGateway // nothing reached upstream — same 502 as traffic
		detail = ferr.Error()
	default:
		detail = res.Err
	}
	ok := status >= 200 && status < 300
	if ok {
		detail = ""
	} else {
		detail = truncate(detail, 500)
	}
	// The probe is a real call and must be recorded like one: invisible spend
	// is unauditable spend. Client "service-test" separates probe rows from
	// client traffic in every dashboard, exactly like "combo-test".
	s.ledger(store.Call{
		TS: start, Client: "service-test", Surface: store2surface(provider.Surface(surface)), Alias: servedRoute.Alias,
		AccountID: servedRoute.Account.ID, Account: servedRoute.Account.Namespace,
		KeyID: servedRoute.KeyID, KeyHint: servedRoute.KeyHint, Model: servedRoute.Model,
		Status: status, Stream: res.Stream, Totalms: res.Totalms,
		EndpointID: res.EndpointID, UpstreamModel: res.UpstreamModel, Err: detail,
	})
	if err := s.db.UpsertServiceProbe(ctx, surface, servedRoute.Account.ID, servedRoute.Model, ok, detail); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"surface":    surface,
		"model":      served,
		"ok":         ok,
		"dispatched": true,
		"status":     status,
		"ms":         ms,
		"detail":     detail,
	})
}

// serviceTargetExists mirrors request-time truth for the UI: a namespaced id
// must be in an ENABLED account's live catalog; a bare name must be an enabled
// combo with hops. Existence only — a transient rules window or cooldown must
// not block writing a default.
func (s *Server) serviceTargetExists(ctx context.Context, target, surface string) (bool, error) {
	if strings.Contains(target, "/") {
		return s.db.ServiceTargetExists(ctx, target)
	}
	// Bare name: a combo. The router resolves bare names to combos only.
	combo, err := s.db.ComboByName(ctx, target)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return combo.Enabled && len(combo.Hops) > 0, nil
}

// serviceProbePath is the UPSTREAM path for a probe, relative to the account's
// base URL (which already carries /v1). provider.SurfacePath is the same
// mapping the dispatcher applies per hop, so this matches what the wire will
// actually see.
func serviceProbePath(surface string) string {
	return provider.SurfacePath(provider.Surface(surface))
}

// serviceProbeBody builds one tiny, endpoint-appropriate request. The STT probe
// is multipart: model field + a 0.2 s silent WAV. An empty prompt falls back to
// the per-surface default; a non-empty one is sent verbatim so the operator can
// test the shape of text they actually care about.
func serviceProbeBody(surface, model, prompt string) ([]byte, string) {
	switch surface {
	case "audio_stt":
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("model", model)
		part, _ := mw.CreateFormFile("file", "test.wav")
		_, _ = part.Write(silentWAV())
		_ = mw.Close()
		return buf.Bytes(), mw.FormDataContentType()
	case "images_gen":
		if prompt == "" {
			prompt = "a small red circle"
		}
		body, _ := json.Marshal(map[string]any{"model": model, "prompt": prompt, "n": 1})
		return body, "application/json"
	case "audio_tts":
		if prompt == "" {
			prompt = "ok"
		}
		body, _ := json.Marshal(map[string]any{"model": model, "input": prompt, "voice": "alloy"})
		return body, "application/json"
	default: // embeddings
		if prompt == "" {
			prompt = "ping"
		}
		body, _ := json.Marshal(map[string]any{"model": model, "input": prompt})
		return body, "application/json"
	}
}

// silentWAV returns a minimal valid WAV: 0.2 s of silence, 8 kHz, 16-bit mono.
func silentWAV() []byte {
	const (
		rate    = 8000
		samples = int(rate * 0.2)
		dataLen = samples * 2
	)
	b := make([]byte, 44+dataLen)
	copy(b[0:], "RIFF")
	le32(b[4:], uint32(36+dataLen))
	copy(b[8:], "WAVEfmt ")
	le32(b[16:], 16)
	le16(b[20:], 1) // PCM
	le16(b[22:], 1) // mono
	le32(b[24:], rate)
	le32(b[28:], rate*2)
	le16(b[32:], 2)
	le16(b[34:], 16)
	copy(b[36:], "data")
	le32(b[40:], uint32(dataLen))
	return b // samples stay zero: silence
}

func le16(b []byte, v uint16) { b[0] = byte(v); b[1] = byte(v >> 8) }
func le32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

// truncate keeps an upstream error detail readable in a verdict without
// shipping a whole provider body into the card.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
