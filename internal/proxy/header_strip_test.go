package proxy

// S6 regression: x-ezllm-compression was forwarded upstream. It is an
// operator-side ledger header — it tells THIS gateway which compression profile
// to run — so it describes the client->gateway hop exactly like
// X-Ezllm-Session, which was already stripped. It was knowingly carried as a
// leak once; there is no reason for a provider to ever see it.
//
// The strip is case-insensitive (copyClientHeaders matches canonical keys and
// folds case for adapter names), so the header is checked the way a client
// may actually send it.

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
			rt := testRoute(up.URL, provider.SurfaceChat)
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
	rt := testRoute(up.URL, provider.SurfaceChat)
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

// copyClientHeaders looks stripAlways up by canonical key, so a non-canonical
// entry would silently never match.
func TestStripAlwaysKeysAreCanonical(t *testing.T) {
	for k := range stripAlways {
		if c := http.CanonicalHeaderKey(k); c != k {
			t.Errorf("stripAlways key %q is not canonical (%q): it would never match", k, c)
		}
	}
}

// Stripping is case-insensitive for both lists, including header keys that
// were stored non-canonically.
func TestCopyClientHeadersStripsAnyCase(t *testing.T) {
	src := httptest.NewRequest("POST", "/", nil)
	src.Header["x-api-key"] = []string{"client-secret"}
	src.Header["cookie"] = []string{"c=1"}
	src.Header.Set("X-Zen-Model", "client-choice")
	src.Header["x-keep"] = []string{"yes"}
	dst := httptest.NewRequest("POST", "/", nil)
	dst.Header = http.Header{}
	copyClientHeaders(dst, src, []string{"x-zen-model"})
	for _, k := range []string{"X-Api-Key", "Cookie", "X-Zen-Model"} {
		if dst.Header.Get(k) != "" {
			t.Errorf("%s leaked upstream", k)
		}
	}
	if got := dst.Header.Values("X-Keep"); len(got) != 1 || got[0] != "yes" {
		t.Errorf("unstripped header lost: %v", dst.Header)
	}
}

// The per-attempt copy builds no strip set: stripping costs no allocation.
func TestCopyClientHeadersStripAllocatesNothing(t *testing.T) {
	src := httptest.NewRequest("POST", "/", nil)
	for _, k := range []string{"Authorization", "Cookie", "Host", "Origin", "X-Ezllm-Session"} {
		src.Header.Set(k, "v")
	}
	dst := httptest.NewRequest("POST", "/", nil)
	extra := []string{"X-Zen-Model"}
	if n := testing.AllocsPerRun(100, func() { copyClientHeaders(dst, src, extra) }); n != 0 {
		t.Errorf("allocs per call = %v, want 0 when every header is stripped", n)
	}
}
