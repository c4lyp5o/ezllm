package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The gemini-web adapter test mocks the upstream the way the geminiweb
// package's own tests do (init page + batchexecute status frames), plus a
// "bridge" endpoint so PrepareRequest's rewrite can be asserted.

const geminiWebInitPage = `<html>{"SNlM0e":"tok-abc123","cfb2h":"boq_lbl","FdrFJe":"sid_42","TuX5cc":"en","qKIAYe":"feeds/push42"}</html>`

func geminiWebStatusFrame(t *testing.T, statusCode int, models []any) string {
	t.Helper()
	body := make([]any, 18)
	body[14] = statusCode
	if models != nil {
		body[15] = models
	}
	bodyJSON, _ := json.Marshal(body)
	// part[2] is a JSON *string* on the real wire — marshal the body once
	// (a second marshal would double-encode it).
	part, _ := json.Marshal([]any{"wrb.fr", "otAQ7b", string(bodyJSON), nil, nil, nil, nil, "generic"})
	units := 0
	for _, r := range string(part) {
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return fmt.Sprintf("%d\n%s", units+1, part)
}

func geminiWebModelDescriptor(id, category, display string, number int) []any {
	d := make([]any, 18)
	d[0] = id
	d[10] = category
	d[11] = display
	d[12] = display + " description"
	d[17] = number
	return d
}

func newGeminiWebMock(t *testing.T, statusCode int, models []any) (*httptest.Server, *httptest.Server) {
	t.Helper()
	// upstream (gemini.google.com stand-in)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app":
			fmt.Fprint(w, geminiWebInitPage)
		case strings.Contains(r.URL.Path, "batchexecute"):
			fmt.Fprint(w, ")]}'\n"+geminiWebStatusFrame(t, statusCode, models))
		default:
			w.WriteHeader(404)
		}
	}))
	// bridge stand-in (records the rewritten request)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "{}")
	}))
	return up, bridge
}

func TestGeminiWebAdapterShape(t *testing.T) {
	up, bridge := newGeminiWebMock(t, 1000, []any{
		geminiWebModelDescriptor("abc123", "Pro", "Gemini 3 Pro", 9),
	})
	defer up.Close()
	defer bridge.Close()

	a := newGeminiWebWithBase(bridge.URL, up.URL, nil)
	if a.Kind() != KindGeminiWeb {
		t.Fatalf("kind = %s", a.Kind())
	}
	if a.CatalogIsPublic() {
		t.Error("catalog must not be public: a dead cookie must 401")
	}
	oracle := a.AuthOracle()
	if oracle.Method != http.MethodGet || oracle.Path != "/models" {
		t.Errorf("oracle = %+v", oracle)
	}
	// StripHeaders removes exactly the three loopback stamps.
	strip := a.StripHeaders()
	if len(strip) != 3 || strip[0] != GeminiWebAccountHeader || strip[1] != GeminiWebKeyHeader || strip[2] != BridgeSecretHeader {
		t.Errorf("strip = %v", strip)
	}
}

func TestGeminiWebPrepareRequestRewrite(t *testing.T) {
	up, bridge := newGeminiWebMock(t, 1000, nil)
	defer up.Close()
	defer bridge.Close()
	a := newGeminiWebWithBase(bridge.URL+"/internal/geminiweb/v1", up.URL, nil)

	req, _ := http.NewRequest(http.MethodPost, "internal://geminiweb/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer client-supplied")
	acct := Account{ID: 42, Kind: KindGeminiWeb}
	if err := a.PrepareRequest(context.Background(), req, acct, "__Secure-1PSID=x\n__Secure-1PSIDTS=y", SurfaceChat); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(req.URL.String(), bridge.URL+"/internal/geminiweb/v1/chat/completions") {
		t.Errorf("rewritten URL = %s", req.URL)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("client Authorization must be dropped (the bridge auths itself)")
	}
	if req.Header.Get(GeminiWebAccountHeader) != "42" {
		t.Errorf("account header = %q", req.Header.Get(GeminiWebAccountHeader))
	}
	if !strings.Contains(req.Header.Get(GeminiWebKeyHeader), "__Secure-1PSID=x") {
		t.Errorf("key header = %q", req.Header.Get(GeminiWebKeyHeader))
	}
}

func TestGeminiWebListModelsAndQuota(t *testing.T) {
	up, bridge := newGeminiWebMock(t, 1000, []any{
		geminiWebModelDescriptor("abc123", "Pro", "Gemini 3 Pro", 9),
		geminiWebModelDescriptor("def456", "Flash", "Gemini 3 Flash", 1),
	})
	defer up.Close()
	defer bridge.Close()
	a := newGeminiWebWithBase(bridge.URL, up.URL, nil)
	key := "__Secure-1PSID=x\n__Secure-1PSIDTS=y"
	acct := Account{ID: 1, Kind: KindGeminiWeb}

	models, err := a.ListModels(context.Background(), acct, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "gemini-pro" || models[1].ID != "gemini-flash" {
		t.Fatalf("models = %+v", models)
	}
	if !strings.Contains(models[0].Display, "gemini-web") {
		t.Errorf("display = %q", models[0].Display)
	}
	q, err := a.ReadQuota(context.Background(), acct, key)
	if err != nil {
		t.Fatal(err)
	}
	if q.Kind != QuotaNone || !strings.Contains(q.Raw, `"status_code":1000`) || !strings.Contains(q.Raw, `"models":2`) {
		t.Errorf("quota = %+v", q)
	}
}

func TestGeminiWebDeadCookie(t *testing.T) {
	up, bridge := newGeminiWebMock(t, 1016, nil)
	defer up.Close()
	defer bridge.Close()
	a := newGeminiWebWithBase(bridge.URL, up.URL, nil)
	acct := Account{ID: 1, Kind: KindGeminiWeb}
	if _, err := a.ListModels(context.Background(), acct, "__Secure-1PSID=stale"); err == nil {
		t.Fatal("unauthenticated status must fail")
	}
	// An expired cookie means Init yields no access token — distinct error.
	upNoToken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>sign in</html>")
	}))
	defer upNoToken.Close()
	a2 := newGeminiWebWithBase(bridge.URL, upNoToken.URL, nil)
	_, err := a2.ListModels(context.Background(), acct, "__Secure-1PSID=stale")
	if err == nil || !strings.Contains(err.Error(), "no SNlM0e") {
		t.Errorf("err = %v, want the expired-cookie signature", err)
	}
}
