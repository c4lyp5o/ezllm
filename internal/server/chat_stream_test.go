package server

// Dashboard chat streaming: POST /admin/chat used to hardcode stream:false, so
// the dashboard could only show an answer once it was fully materialised. The
// endpoint must forward the caller's stream flag untouched — the dispatcher
// already knows how to relay SSE byte-for-byte.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAdminChatForwardsStreamFlag(t *testing.T) {
	var upstreamStream *bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
			return
		}
		var got struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("decode upstream payload %q: %v", raw, err)
			return
		}
		upstreamStream = &got.Stream
		if got.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	})

	const msg = `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`

	code, hdr, body := h.do("POST", "/admin/chat", "tok-admin", strings.TrimSuffix(msg, "}")+`,"stream":true}`)
	if code != http.StatusOK {
		t.Fatalf("stream=true: status %d body %s", code, body)
	}
	if upstreamStream == nil || !*upstreamStream {
		t.Fatalf("upstream saw stream=%v, want true", upstreamStream)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body lost the SSE terminator: %q", body)
	}

	code, _, body = h.do("POST", "/admin/chat", "tok-admin", msg)
	if code != http.StatusOK {
		t.Fatalf("stream absent: status %d body %s", code, body)
	}
	if upstreamStream == nil || *upstreamStream {
		t.Fatalf("upstream saw stream=%v, want false (default)", upstreamStream)
	}
	if strings.Contains(body, "data:") {
		t.Errorf("non-stream reply leaked SSE framing: %q", body)
	}
}
