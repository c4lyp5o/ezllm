package server

// Services slice (M10): per-service default bindings, honest first-run state,
// typed refusals, and endpoint-appropriate probes. Everything here runs against
// the real handler stack — no mocked handlers, no mocked dispatch.

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// servicesGET fetches /admin/services and decodes the card list.
func servicesGET(t *testing.T, h *harness) map[string]any {
	t.Helper()
	code, _, body := h.do("GET", "/admin/services", "tok-admin", "")
	if code != 200 {
		t.Fatalf("GET /admin/services -> %d: %s", code, body)
	}
	var out struct {
		Services []map[string]any `json:"services"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	bySurface := map[string]any{}
	for _, s := range out.Services {
		bySurface[s["surface"].(string)] = s
	}
	return bySurface
}

func TestServicesListAlwaysShowsAllFour(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	cards := servicesGET(t, h)
	// All four surfaces exist as cards on a first run, even with nothing
	// configured — the page teaches instead of hiding.
	for _, surface := range []string{"embeddings", "images_gen", "audio_tts", "audio_stt"} {
		card, ok := cards[surface]
		if !ok {
			t.Fatalf("surface %q missing from GET /admin/services", surface)
		}
		if card.(map[string]any)["mode"] != "off" {
			t.Errorf("%s first-run mode = %v, want off", surface, card)
		}
		if card.(map[string]any)["verdict"] != nil {
			t.Errorf("%s claims a verdict with zero probes: %v", surface, card)
		}
		if card.(map[string]any)["endpoint"] == "" {
			t.Errorf("%s card has no endpoint label: %v", surface, card)
		}
	}
	if cards["embeddings"].(map[string]any)["accounts"].(float64) < 1 {
		t.Errorf("accounts count should see the harness provider: %v", cards["embeddings"])
	}
}

func TestServiceBindingManualThenDefaultedRequest(t *testing.T) {
	// The upstream records what model actually reached it — the whole point of
	// the default is that a model-less request carries the bound one.
	var gotModel string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&m)
		gotModel = m.Model
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"embedding":[0.1]}]}`))
	})

	// Bind manually.
	code, _, body := h.do("PUT", "/admin/services/embeddings", "tok-admin",
		`{"mode":"manual","model":"up/gpt-6-luna"}`)
	if code != 200 {
		t.Fatalf("PUT binding -> %d: %s", code, body)
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(body), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if card["mode"] != "manual" || card["resolved_target"] != "up/gpt-6-luna" {
		t.Fatalf("card after bind = %v", card)
	}
	if card["target_exists"] != true {
		t.Fatalf("bound model should validate as existing: %v", card)
	}

	// A model-less request now rides the binding. The upstream sees the hop's
	// BARE model id (the proxy swaps namespaced aliases per hop) — what matters
	// is that the bound model is the one that got there, and the only model
	// this harness serves is gpt-6-luna.
	code, _, body = h.do("POST", "/v1/embeddings", "tok-infer", `{"input":"hello"}`)
	if code != 200 {
		t.Fatalf("defaulted embeddings -> %d: %s", code, body)
	}
	if gotModel != "gpt-6-luna" {
		t.Fatalf("upstream saw model %q, want the bound default's bare id gpt-6-luna", gotModel)
	}
}

func TestServiceExplicitModelBeatsBinding(t *testing.T) {
	var gotModel string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&m)
		gotModel = m.Model
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"embedding":[0.1]}]}`))
	})
	// A SECOND model in the catalog: the binding points at one, the request at
	// the other, so the assertion can tell them apart at the wire.
	if err := h.db.UpsertModels(context.Background(), 1, []provider.ModelInfo{
		{ID: "gpt-6-luna", OwnedBy: "up"}, {ID: "explicit-1", OwnedBy: "up"},
	}); err != nil {
		t.Fatal(err)
	}
	if code, _, body := h.do("PUT", "/admin/services/embeddings", "tok-admin",
		`{"mode":"manual","model":"up/gpt-6-luna"}`); code != 200 {
		t.Fatalf("bind: %d %s", code, body)
	}
	// The request's own model is authoritative — the binding is a DEFAULT.
	if code, _, body := h.do("POST", "/v1/embeddings", "tok-infer",
		`{"input":"hi","model":"up/explicit-1"}`); code != 200 {
		t.Fatalf("explicit-model request -> %d: %s", code, body)
	}
	if gotModel != "explicit-1" {
		t.Fatalf("upstream saw %q, want explicit-1 (the request's model must win over the binding)", gotModel)
	}
}

func TestServiceNotConfiguredIsTypedRefusal(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be consulted for an unconfigured service")
	})
	code, _, body := h.do("POST", "/v1/embeddings", "tok-infer", `{"input":"hi"}`)
	if code != 400 {
		t.Fatalf("unconfigured service -> %d, want 400 typed refusal: %s", code, body)
	}
	if !strings.Contains(body, "service_not_configured") {
		t.Errorf("refusal must carry its type, got: %s", body)
	}
	if !strings.Contains(body, "/services") {
		t.Errorf("refusal must point at the Services page, got: %s", body)
	}
	// The refusal is a real event and must be visible in the ledger.
	h.db.Flush()
	rows, _ := h.db.RecentCalls(context.Background(), 5)
	if len(rows) == 0 {
		t.Fatal("unconfigured refusal was not ledgered")
	}
	if rows[0].Status != 400 {
		t.Errorf("ledger row status = %d, want 400", rows[0].Status)
	}
}

func TestServiceBindingValidation(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	// Unknown surface → 400 with the allowed list.
	code, _, body := h.do("PUT", "/admin/services/nope", "tok-admin", `{"mode":"manual","model":"up/m"}`)
	if code != 400 || !strings.Contains(body, "embeddings") {
		t.Errorf("unknown surface -> %d %s, want 400 naming the surfaces", code, body)
	}

	// Unknown model → 404, not silently accepted.
	code, _, body = h.do("PUT", "/admin/services/embeddings", "tok-admin",
		`{"mode":"manual","model":"up/ghost-model"}`)
	if code != 404 || !strings.Contains(body, "model_not_found") {
		t.Errorf("unknown model -> %d %s, want 404 model_not_found", code, body)
	}

	// Manual without a model → 400.
	if code, _, body := h.do("PUT", "/admin/services/embeddings", "tok-admin", `{"mode":"manual"}`); code != 400 {
		t.Errorf("manual without model -> %d %s, want 400", code, body)
	}

	// Garbage mode → 400.
	if code, _, body := h.do("PUT", "/admin/services/embeddings", "tok-admin", `{"mode":"chaos"}`); code != 400 {
		t.Errorf("bad mode -> %d %s, want 400", code, body)
	}

	// Unknown fields are rejected (strict decode).
	if code, _, body := h.do("PUT", "/admin/services/embeddings", "tok-admin",
		`{"mode":"auto","suface":"typo"}`); code != 400 {
		t.Errorf("typo'd key accepted -> %d %s", code, body)
	}

	// A model-less request on an unbound, unconfigured surface refuses typed.
	code, _, body = h.do("POST", "/v1/embeddings", "tok-infer", `{"input":"x"}`)
	if code != 400 || !strings.Contains(body, "service_not_configured") {
		t.Errorf("unbound surface -> %d %s, want typed refusal", code, body)
	}
}

func TestServiceAutoWaitsForProbes(t *testing.T) {
	// Auto must not guess: with zero successful probes it behaves as
	// unconfigured even when the binding is set to auto.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"embedding":[0.1]}]}`))
	})
	if code, _, body := h.do("PUT", "/admin/services/embeddings", "tok-admin", `{"mode":"auto"}`); code != 200 {
		t.Fatalf("bind auto: %d %s", code, body)
	}
	code, _, body := h.do("POST", "/v1/embeddings", "tok-infer", `{"input":"hi"}`)
	if code != 400 || !strings.Contains(body, "service_not_configured") {
		t.Fatalf("auto with no probes must refuse, got %d %s", code, body)
	}

	// A green probe proves the model answers this surface's shape → auto may
	// use it now.
	code, _, body = h.do("POST", "/admin/services/embeddings/test", "tok-admin",
		`{"model":"up/gpt-6-luna"}`)
	if code != 200 {
		t.Fatalf("probe -> %d: %s", code, body)
	}
	var verdict map[string]any
	if err := json.Unmarshal([]byte(body), &verdict); err != nil {
		t.Fatalf("decode verdict: %v", err)
	}
	if verdict["ok"] != true {
		t.Fatalf("probe should pass against the working fake upstream: %v", verdict)
	}

	code, _, body = h.do("POST", "/v1/embeddings", "tok-infer", `{"input":"hi"}`)
	if code != 200 {
		t.Fatalf("auto after probe -> %d: %s", code, body)
	}
	// And the card now shows an honest verdict.
	cards := servicesGET(t, h)
	card := cards["embeddings"].(map[string]any)
	if card["verdict"] == nil {
		t.Fatalf("card has no verdict after a green probe: %v", card)
	}
	if card["verified_models"].(float64) != 1 {
		t.Errorf("verified_models = %v, want 1", card["verified_models"])
	}
}

func TestServiceTestRecordsFailureVerdict(t *testing.T) {
	// A failing probe is a real result: it must land as an honest failed
	// verdict, not a silent no-op, and must not feed Auto.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		w.Write([]byte(`{"error":{"message":"capacity"}}`))
	})
	code, _, body := h.do("POST", "/admin/services/embeddings/test", "tok-admin",
		`{"model":"up/gpt-6-luna"}`)
	if code != 200 {
		t.Fatalf("probe endpoint itself must succeed in recording, got %d %s", code, body)
	}
	var verdict map[string]any
	if err := json.Unmarshal([]byte(body), &verdict); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if verdict["ok"] != false {
		t.Fatalf("probe against a 503 upstream must be ok=false: %v", verdict)
	}
	// Auto must still refuse — no tested-ok model exists.
	if code, _, body := h.do("PUT", "/admin/services/embeddings", "tok-admin", `{"mode":"auto"}`); code != 200 {
		t.Fatalf("bind auto: %d %s", code, body)
	}
	if code, _, body := h.do("POST", "/v1/embeddings", "tok-infer", `{"input":"x"}`); code != 400 {
		t.Fatalf("auto with only failed probes must refuse, got %d %s", code, body)
	}
	// The probe is admin traffic: it leaves no ledger row (the refused client
	// request above is real traffic and stays).
	h.db.Flush()
	rows, _ := h.db.RecentCalls(context.Background(), 5)
	for _, row := range rows {
		if row.Client == "service-test" {
			t.Errorf("service probe reached the ledger: %+v", row)
		}
	}
}

func TestServiceChatStillRequiresModel(t *testing.T) {
	// The chat surfaces keep their contract: a model-less request is a plain
	// 400 'model' is required — services defaulting must not leak into chat.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("POST", "/v1/chat/completions", "tok-infer", `{"messages":[]}`)
	if code != 400 || !strings.Contains(body, "model") {
		t.Fatalf("model-less chat -> %d %s, want 400 requiring a model", code, body)
	}
	// Same for the other non-service surfaces. (/v1/messages is the anthropic
	// route — /anthropic/v1/messages is unregistered by design.)
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		code, _, body = h.do("POST", path, "tok-infer", `{"messages":[]}`)
		if code != 400 {
			t.Errorf("model-less %s -> %d %s, want 400", path, code, body)
		}
	}
}

func TestServiceSTTDefaultsAndInjectsModel(t *testing.T) {
	// A model-less multipart transcription must carry the bound model into the
	// upstream form — the field is injected per hop, not left absent.
	var gotModel string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			gotModel = r.FormValue("model")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":"hello"}`))
	})
	if code, _, body := h.do("PUT", "/admin/services/audio_stt", "tok-admin",
		`{"mode":"manual","model":"up/gpt-6-luna"}`); code != 200 {
		t.Fatalf("bind stt: %d %s", code, body)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.wav")
	fw.Write(silentWAV())
	mw.Close()
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", &buf)
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("model-less stt -> %d: %s", rec.Code, rec.Body.String())
	}
	// The upstream sees the injected model as the hop's bare id (per-hop swap).
	if gotModel != "gpt-6-luna" {
		t.Fatalf("upstream form model = %q, want the injected bound default's bare id", gotModel)
	}

	// And with no binding at all it refuses typed, upstream untouched.
	h2 := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be consulted for an unconfigured service")
	})
	buf.Reset()
	mw = multipart.NewWriter(&buf)
	fw, _ = mw.CreateFormFile("file", "a.wav")
	fw.Write(silentWAV())
	mw.Close()
	req = httptest.NewRequest("POST", "/v1/audio/transcriptions", &buf)
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec = httptest.NewRecorder()
	h2.handler.ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "service_not_configured") {
		t.Fatalf("unconfigured stt -> %d %s, want typed refusal", rec.Code, rec.Body.String())
	}
}

func TestServicesRequireAdmin(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	// The service config surface is admin territory; infer tokens get 403.
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/admin/services", ""},
		{"PUT", "/admin/services/embeddings", `{"mode":"auto"}`},
		{"POST", "/admin/services/embeddings/test", `{"model":"up/gpt-6-luna"}`},
	} {
		code, _, _ := h.do(tc.method, tc.path, "tok-infer", tc.body)
		if code != 403 {
			t.Errorf("%s %s with infer token -> %d, want 403", tc.method, tc.path, code)
		}
		if code, _, _ := h.do(tc.method, tc.path, "", tc.body); code != 401 {
			t.Errorf("%s %s unauthenticated -> %d, want 401", tc.method, tc.path, code)
		}
	}
}

func TestServiceTestCustomPromptReachesUpstream(t *testing.T) {
	var seen map[string]any
	seenPath := ""
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seen = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"embedding":[0.1]}]}`))
	})

	// A custom prompt goes out verbatim on the endpoint's own wire field.
	code, _, body := h.do("POST", "/admin/services/images_gen/test", "tok-admin",
		`{"model":"up/gpt-6-luna","prompt":"a mango on a blue plate"}`)
	if code != 200 {
		t.Fatalf("images probe -> %d: %s", code, body)
	}
	if seenPath != "/images/generations" {
		t.Errorf("probe hit %q, want /images/generations (upstream path)", seenPath)
	}
	if seen["prompt"] != "a mango on a blue plate" {
		t.Errorf("upstream prompt = %v, want the custom prompt verbatim", seen["prompt"])
	}
	if n, _ := seen["n"].(float64); n != 1 {
		t.Errorf("upstream n = %v, want 1 (probe cost control)", seen["n"])
	}

	// Omitted prompt falls back to the per-surface default, not an empty string.
	seen = nil
	code, _, body = h.do("POST", "/admin/services/embeddings/test", "tok-admin", `{"model":"up/gpt-6-luna"}`)
	if code != 200 {
		t.Fatalf("embeddings probe -> %d: %s", code, body)
	}
	if seen["input"] != "ping" {
		t.Errorf("upstream input = %v, want the default 'ping'", seen["input"])
	}

	// A prompt on the file-upload surface is refused, never silently dropped.
	code, _, body = h.do("POST", "/admin/services/audio_stt/test", "tok-admin",
		`{"model":"up/gpt-6-luna","prompt":"hello"}`)
	if code != 400 || !strings.Contains(body, "file upload") {
		t.Errorf("stt probe with prompt -> %d %s, want 400 explaining the file-upload shape", code, body)
	}

	// This is a wire-shape test, not a workload: the prompt is bounded.
	long := strings.Repeat("x", 2001)
	code, _, body = h.do("POST", "/admin/services/embeddings/test", "tok-admin",
		`{"model":"up/gpt-6-luna","prompt":"`+long+`"}`)
	if code != 400 {
		t.Errorf("oversized prompt -> %d %s, want 400", code, body)
	}
}

// A service probe is real spend, but admin traffic: it is not ledgered.
func TestServiceTestIsNotLedgered(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":7,"total_tokens":7}}`))
	})
	if code, _, body := h.do("POST", "/admin/services/embeddings/test", "tok-admin", `{"model":"up/gpt-6-luna"}`); code != 200 {
		t.Fatalf("probe: %d %s", code, body)
	}
	h.db.Flush()
	if rows, _ := h.db.RecentCalls(context.Background(), 5); len(rows) != 0 {
		t.Fatalf("service probe reached the ledger: %+v", rows)
	}
}
