package proxy

// S6 regression: x-ezllm-compression was forwarded upstream. It is an
// operator-side ledger header — it tells THIS gateway which compression profile
// to run — so it describes the client->gateway hop exactly like
// X-Ezllm-Session, which was already stripped. It was knowingly carried as a
// leak once; there is no reason for a provider to ever see it.
//
// The strip is case-insensitive (copyClientHeaders lowercases before lookup),
// so the header is checked the way a client may actually send it.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestEzllmHeadersAreStrippedBeforeUpstream(t *testing.T) {
	for _, hdr := range []string{
		"X-Ezllm-Session",
		"X-Ezllm-Compression",
		"x-ezllm-session",
		"x-ezllm-compression",
	} {
		t.Run(hdr, func(t *testing.T) {
			var got http.Header
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer up.Close()

			d := NewDispatcher(up.Client(), provider.NewRegistry(up.Client()))
			rt := testRoute(up.URL, provider.SurfaceOpenAI)
			req, _ := reqWithBody(`{"model":"m","messages":[]}`)
			req.Header.Set(hdr, "operator-value")

			rec := httptest.NewRecorder()
			if _, err := d.Forward(context.Background(), rec, req, rt,
				[]byte(`{"model":"m","messages":[]}`)); err != nil {
				t.Fatalf("forward: %v", err)
			}
			for name := range got {
				if len(name) >= 6 && (name[:6] == "X-Ezll" || name[:6] == "x-ezll") {
					t.Errorf("header %q reached the upstream (value %q) — ezllm operator headers must die at the gateway", name, got.Get(name))
				}
			}
		})
	}
}

// The client's own provider headers still pass, plus custom per-account ones:
// a strip list that is too aggressive would silently break auth upstream.
func TestClientAndCustomHeadersStillForward(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	d := NewDispatcher(up.Client(), provider.NewRegistry(up.Client()))
	rt := testRoute(up.URL, provider.SurfaceOpenAI)
	req, _ := reqWithBody(`{"model":"m","messages":[]}`)
	req.Header.Set("User-Agent", "ezllm-test/1.0")
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	if _, err := d.Forward(context.Background(), rec, req, rt,
		[]byte(`{"model":"m","messages":[]}`)); err != nil {
		t.Fatalf("forward: %v", err)
	}
	if got.Get("User-Agent") != "ezllm-test/1.0" {
		t.Errorf("User-Agent lost: %q", got.Get("User-Agent"))
	}
	if got.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type lost: %q", got.Get("Content-Type"))
	}
}
