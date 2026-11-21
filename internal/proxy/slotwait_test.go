package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/bridgeutil"
	"github.com/c4lyp5o/ezllm/internal/provider"
)

// A loopback bridge's slot wait reaches the Result (and so the ledger), and
// the internal header is not passed on to the client.
func TestForwardRecordsBridgeSlotWait(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(bridgeutil.SlotWaitHeader, "1234")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer upstream.Close()

	d := newDispatcher()
	body := []byte(`{"model":"resolved-model","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	res, err := d.Forward(context.Background(), rec, req, testRoute(upstream.URL, provider.SurfaceChat), body)
	if err != nil {
		t.Fatal(err)
	}
	if res.SlotWaitms == nil || *res.SlotWaitms != 1234 {
		t.Fatalf("SlotWaitms = %v, want 1234", res.SlotWaitms)
	}
	if v := rec.Header().Get(bridgeutil.SlotWaitHeader); v != "" {
		t.Fatalf("client received the internal header: %q", v)
	}
}

// Only a loopback upstream may write the column; garbage is ignored.
func TestBridgeSlotWaitTrustsOnlyLoopback(t *testing.T) {
	h := http.Header{}
	h.Set(bridgeutil.SlotWaitHeader, "250")
	for _, tc := range []struct {
		base string
		want bool
	}{
		{"http://127.0.0.1:8080/v1", true},
		{"http://[::1]:8080/v1", true},
		{"http://localhost:8080/v1", true},
		{"https://api.example.com/v1", false},
		{"http://10.0.0.5/v1", false},
	} {
		u, _ := url.Parse(tc.base)
		if got := bridgeSlotWait(u, h) != nil; got != tc.want {
			t.Errorf("%s: trusted = %v, want %v", tc.base, got, tc.want)
		}
	}
	u, _ := url.Parse("http://127.0.0.1/v1")
	for _, bad := range []string{"-5", "soon", "1.5"} {
		h.Set(bridgeutil.SlotWaitHeader, bad)
		if got := bridgeSlotWait(u, h); got != nil {
			t.Errorf("%q parsed to %d, want nil", bad, *got)
		}
	}
}
