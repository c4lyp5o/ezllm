package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The Services card's "Try it" runs the operator's model-less request through
// the real pipeline with the service DEFAULT as the model, returns the
// upstream bytes untouched (audio stays audio), and is admin-only. Services
// whose output is not media (embeddings, moderations, rerank) are refused.
func TestServiceGenerateUsesTheDefaultAndPassesOutputThrough(t *testing.T) {
	var gotPath, gotModel string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &m)
		gotPath, gotModel = r.URL.Path, m.Model
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("ID3-fake-mp3-bytes"))
	})
	const body = `{"input":"hello","voice":"alloy"}`

	// No default yet: refused, nothing dialed.
	if code, _, b := h.do("POST", "/admin/services/audio_tts/generate", "tok-admin", body); code == http.StatusOK || gotPath != "" {
		t.Fatalf("no default: status %d body %s, upstream path %q — want a refusal and no dial", code, b, gotPath)
	}

	if err := h.db.SetServiceBinding(context.Background(), "audio_tts", "manual", "up/gpt-6-luna"); err != nil {
		t.Fatal(err)
	}
	code, hdr, b := h.do("POST", "/admin/services/audio_tts/generate", "tok-admin", body)
	if code != http.StatusOK {
		t.Fatalf("status %d body %s", code, b)
	}
	if gotPath != "/audio/speech" || gotModel != "gpt-6-luna" {
		t.Errorf("upstream saw path %q model %q, want /audio/speech + the default's upstream id", gotPath, gotModel)
	}
	if ct := hdr.Get("Content-Type"); ct != "audio/mpeg" || b != "ID3-fake-mp3-bytes" {
		t.Errorf("response = %q (%s), want the upstream audio bytes untouched", b, ct)
	}

	for _, svc := range []string{"embeddings", "moderations", "rerank", "nope"} {
		if code, _, b := h.do("POST", "/admin/services/"+svc+"/generate", "tok-admin", body); code != http.StatusBadRequest || !strings.Contains(b, "cannot generate") {
			t.Errorf("%s: status %d body %s, want 400 cannot generate", svc, code, b)
		}
	}
	if code, _, _ := h.do("POST", "/admin/services/audio_tts/generate", "tok-infer", body); code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Errorf("infer token: status %d, want admin-only", code)
	}
}
