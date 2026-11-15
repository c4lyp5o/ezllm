package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/config"
)

// The in-process bridges dial the server's own listener. A wildcard bind
// (0.0.0.0, [::], ":port") is not an address to dial; loopback is.
func TestBridgeBaseIsLoopbackForWildcardBinds(t *testing.T) {
	for addr, want := range map[string]string{
		":20129":         "http://127.0.0.1:20129",
		"0.0.0.0:20129":  "http://127.0.0.1:20129",
		"[::]:20129":     "http://127.0.0.1:20129",
		"127.0.0.1:9000": "http://127.0.0.1:9000",
		"10.0.0.5:20129": "http://10.0.0.5:20129",
	} {
		if got := bridgeOrigin(addr); got != want {
			t.Errorf("bridgeOrigin(%q) = %q, want %q", addr, got, want)
		}
	}
}

// EZLLM_TRANSLATION_ENABLED is the one switch, and a malformed value fails
// the boot.
func TestTranslationFromEnv(t *testing.T) {
	cases := []struct {
		name        string
		env         map[string]string
		start, want bool
		wantErr     bool
	}{
		{"unset keeps config", nil, true, true, false},
		{"off", map[string]string{"EZLLM_TRANSLATION_ENABLED": "false"}, true, false, false},
		{"on overrides config off", map[string]string{"EZLLM_TRANSLATION_ENABLED": "1"}, false, true, false},
		{"malformed fails", map[string]string{"EZLLM_TRANSLATION_ENABLED": "maybe"}, true, true, true},
	}
	for _, c := range cases {
		tr := config.Translation{Enabled: c.start, UniversalEnabled: c.start}
		err := translationFromEnv(&tr, func(k string) string { return c.env[k] })
		if (err != nil) != c.wantErr {
			t.Fatalf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
		}
		if !c.wantErr && (tr.Enabled != c.want || tr.UniversalEnabled != c.want) {
			t.Errorf("%s: enabled=%v universal=%v, want both %v", c.name, tr.Enabled, tr.UniversalEnabled, c.want)
		}
	}
}

// Every provider call goes through upstreamClient; a redirect to another
// origin must not carry the key header or the prompt there.
func TestUpstreamClientRefusesCrossOriginRedirects(t *testing.T) {
	var leaked bool
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer collector.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, collector.URL, http.StatusTemporaryRedirect)
	}))
	defer up.Close()
	req, _ := http.NewRequest("POST", up.URL, strings.NewReader("PRIVATE_PROMPT_MARKER"))
	req.Header.Set("X-Api-Key", "dummy-provider-key")
	resp, err := upstreamClient(&config.Config{}).Do(req)
	if err == nil {
		resp.Body.Close()
	}
	if err == nil || leaked {
		t.Fatalf("redirect to another origin followed: err=%v leaked=%v", err, leaked)
	}
}
