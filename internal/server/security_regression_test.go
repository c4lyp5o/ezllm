package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestSecurityRegressionVSCodeRefusalsRedactCredentials(t *testing.T) {
	h, sink := harnessWithLog(t, slog.LevelDebug, okUpstream)
	const credential = "synthetic-disabled-credential"
	id, _, err := h.db.UpsertToken(context.Background(), store.TokenInput{Name: "audit-key", Roles: []string{"infer"}, Token: credential})
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	if err = h.db.UpdateToken(context.Background(), id, &disabled, nil, nil, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	code, _, _ := h.do("GET", "/api/v1/vscode/"+credential+"/models", "", "")
	if code != 401 {
		t.Fatal(code)
	}
	if strings.Contains(sink.String(), credential) {
		t.Fatal("credential leaked into refusal log")
	}
	if !strings.Contains(sink.String(), "request refused") || !strings.Contains(sink.String(), "/api/v1/vscode/***/models") {
		t.Fatal("redacted diagnostic missing")
	}
}

func TestSecurityRegressionPanicLogOmitsCredentialAndPanicPayload(t *testing.T) {
	h, sink := harnessWithLog(t, slog.LevelError, okUpstream)
	const credential = "synthetic-panic-credential"
	request := httptest.NewRequest("GET", "/api/v1/vscode/"+credential+"/models", nil)
	response := httptest.NewRecorder()
	h.server.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(credential) })).ServeHTTP(response, request)
	if response.Code != 500 {
		t.Fatal(response.Code)
	}
	if strings.Contains(sink.String(), credential) {
		t.Fatal("credential leaked in panic path or payload")
	}
	if !strings.Contains(sink.String(), "panic") || !strings.Contains(sink.String(), "/api/v1/vscode/***/models") {
		t.Fatal("redacted panic diagnostic missing")
	}
}

func TestSecurityRegressionProviderCannotSetGatewayCookies(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/admin/chat"} {
		t.Run(path, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Set-Cookie", "ezllm_session=provider-controlled; Path=/admin; HttpOnly; SameSite=Strict")
				w.Header().Set("Clear-Site-Data", `"cookies", "storage"`)
				w.Header().Set("Retry-After", "7")
				okUpstream(w, r)
			})
			gateway := httptest.NewServer(h.handler)
			defer gateway.Close()
			jar, _ := cookiejar.New(nil)
			client := &http.Client{Jar: jar, Timeout: 3 * time.Second}
			req, _ := http.NewRequest("POST", gateway.URL+path, strings.NewReader(`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hello"}]}`))
			token := "tok-infer"
			if path == "/admin/chat" {
				token = "tok-admin"
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatal(res.StatusCode)
			}
			if res.Header.Get("Set-Cookie") != "" || res.Header.Get("Clear-Site-Data") != "" {
				t.Fatal("provider origin-state headers forwarded")
			}
			if res.Header.Get("Retry-After") != "7" {
				t.Fatal("provider rate-limit header lost")
			}
			target, _ := url.Parse(gateway.URL + "/admin/accounts")
			for _, c := range jar.Cookies(target) {
				if c.Name == "ezllm_session" {
					t.Fatal("provider cookie accepted for gateway admin path")
				}
			}
		})
	}
}

func TestSecurityRegressionSettingsBodyReadDeadline(t *testing.T) {
	old := adminBodyTimeout
	adminBodyTimeout = 100 * time.Millisecond
	defer func() { adminBodyTimeout = old }()
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	gateway := httptest.NewServer(h.handler)
	defer gateway.Close()
	c, err := net.Dial("tcp", strings.TrimPrefix(gateway.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "PATCH /admin/settings HTTP/1.1\r\nHost: gateway\r\nAuthorization: Bearer tok-admin\r\nContent-Length: 100\r\nContent-Type: application/json\r\n\r\n{\"revision\"")
	c.SetReadDeadline(time.Now().Add(time.Second))
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("settings body deadline did not end request: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatal(res.StatusCode)
	}
}

func TestSecurityRegressionEncodedPathCredentialIsFullyRedacted(t *testing.T) {
	h, sink := harnessWithLog(t, slog.LevelDebug, okUpstream)
	const credential = "synthetic/credential-tail"
	id, _, err := h.db.UpsertToken(context.Background(), store.TokenInput{Name: "encoded-key", Roles: []string{"infer"}, Token: credential})
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	if err = h.db.UpdateToken(context.Background(), id, &disabled, nil, nil, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	code, _, _ := h.do("GET", "/api/v1/%76scode/"+url.PathEscape(credential)+"/models", "", "")
	if code != 401 {
		t.Fatal(code)
	}
	if strings.Contains(sink.String(), "credential-tail") || !strings.Contains(sink.String(), "/api/v1/vscode/***/models") {
		t.Fatal("encoded credential leaked or diagnostic lost")
	}
}
