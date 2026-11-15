package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestSecurityRegressionCallerIdentityOnlyReachesRegisteredBridge(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		register, trusted, correctCredential bool
		want                                 string
	}{
		{"gateway bridge", true, true, true, "authenticated-caller"},
		{"spoofed external header", true, false, true, ""},
		{"remote provider", false, true, true, ""},
		{"wrong bridge credential", true, true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan string, 1)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- r.Header.Get(provider.BridgeCallerHeader)
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"ok":true}`))
			}))
			defer up.Close()
			d := NewDispatcher(up.Client(), provider.NewRegistry(up.Client()))
			rt := testRoute(up.URL, provider.SurfaceChat)
			if tc.register {
				token := rt.KeyPlain
				if !tc.correctCredential {
					token = "different-secret"
				}
				d.RegisterCallerBridge(up.URL, token)
			}
			req, _ := reqWithBody(`{"model":"m","messages":[]}`)
			req.Header.Set(provider.BridgeCallerHeader, "spoofed-caller")
			ctx := context.Background()
			if tc.trusted {
				ctx = WithCaller(ctx, "authenticated-caller")
			}
			if _, err := d.Forward(ctx, httptest.NewRecorder(), req, rt, []byte(`{"model":"m","messages":[]}`)); err != nil {
				t.Fatal(err)
			}
			if id := <-got; id != tc.want {
				t.Fatalf("caller=%q want=%q", id, tc.want)
			}
		})
	}
}
