package provider

// Cloudflare Workers AI — host-scoped behaviour measured live 2026-10-13.
// See internal/registration/cloudflare_probe_test.go for the end-to-end key
// test (oracle URL + calibration + native envelope + inference).

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIsCloudflareAIDetection(t *testing.T) {
	yes := []string{
		"https://api.cloudflare.com/client/v4/accounts/9f3c2e1d4b5a69788796a5b4c3d2e1f0/ai/v1",
		"https://api.cloudflare.com/client/v4/accounts/abc/ai/v1/",
	}
	for _, base := range yes {
		acct := testAccount(KindOpenAICompatible, base)
		if !isCloudflareAI(acct) {
			t.Fatalf("must detect %q", base)
		}
	}
	no := []struct {
		kind Kind
		base string
	}{
		{KindOpenAICompatible, "https://api.openai.com/v1"},
		{KindOpenAICompatible, "https://api.cloudflare.com/client/v4/accounts/abc/ai/v1/models"},           // wrong suffix
		{KindOpenAICompatible, "http://api.cloudflare.com/client/v4/accounts/abc/ai/v1"},                   // not https (isOllamaCloud parity)
		{KindAnthropicCompat, "https://api.cloudflare.com/client/v4/accounts/abc/ai/v1"},                   // wrong kind
		{KindOpenAICompatible, "https://evil.example.com/api.cloudflare.com/client/v4/accounts/abc/ai/v1"}, // hostname, not substring
	}
	for _, c := range no {
		acct := testAccount(c.kind, c.base)
		if isCloudflareAI(acct) {
			t.Fatalf("must not detect kind=%s base=%q", c.kind, c.base)
		}
	}
}

func TestAuthOracleForCloudflareIsAbsoluteWithCalibrationKey(t *testing.T) {
	a := NewOpenAICompatible(&http.Client{})
	acct := testAccount(KindOpenAICompatible,
		"https://api.cloudflare.com/client/v4/accounts/9f3c2e1d4b5a69788796a5b4c3d2e1f0/ai/v1")
	hosted, ok := a.(HostedAuthOracle)
	if !ok {
		t.Fatal("openai-compatible must implement HostedAuthOracle")
	}
	o := hosted.AuthOracleFor(acct)
	if o.Path != "https://api.cloudflare.com/client/v4/user/tokens/verify" {
		t.Fatalf("oracle path = %q, want the absolute tokens/verify URL", o.Path)
	}
	if o.Method != http.MethodGet {
		t.Fatalf("oracle method = %q, want GET", o.Method)
	}
	// Calibration key must be cfut_ + 40 hex: Cloudflare rejects any other
	// SHAPE with 400 before looking at the credential (live-verified), and a
	// 400 calibration reads as inconclusive → deferred → wasted proof.
	if !strings.HasPrefix(o.CalibrationKey, "cfut_") || len(o.CalibrationKey) != len("cfut_")+40 {
		t.Fatalf("CalibrationKey = %q…(%d chars), want cfut_ + 40 hex", o.CalibrationKey[:5], len(o.CalibrationKey))
	}
	const hexchars = "0123456789abcdef"
	for _, r := range o.CalibrationKey[len("cfut_"):] {
		if !strings.ContainsRune(hexchars, r) {
			t.Fatalf("CalibrationKey suffix is not hex: %q", o.CalibrationKey)
		}
	}
}

type cfCatalogTransport struct{ key string }

func cfCatalogJSON(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func (tr cfCatalogTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Path, "/ai/models/search") {
		return cfCatalogJSON(405, `{"result":null,"success":false,"errors":[{"code":7001,"message":"GET not supported for requested URI."}],"messages":[]}`), nil
	}
	if req.Header.Get("Authorization") != "Bearer "+tr.key {
		return cfCatalogJSON(401, `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}],"messages":[],"result":null}`), nil
	}
	return cfCatalogJSON(200, `{"errors":[],"messages":[],"success":true,"result":[
		{"id":"@cf/zai-org/glm-4.7-flash","name":"GLM-4.7 Flash"},
		{"id":"@cf/baai/bge-large-en-v1.5","name":"BGE Large (embedding)"},
		{"id":"","name":"entry without id must be skipped"},
		{"id":"@cf/zai-org/glm-4.7-flash","name":"duplicate"}
	]}`), nil
}

func TestListModelsCloudflareEnvelope(t *testing.T) {
	const key = "cfut_live_token"
	c := &http.Client{Transport: cfCatalogTransport{key: key}}
	acct := testAccount(KindOpenAICompatible,
		"https://api.cloudflare.com/client/v4/accounts/9f3c2e1d4b5a69788796a5b4c3d2e1f0/ai/v1")

	a := NewOpenAICompatible(c)
	got, err := a.ListModels(context.Background(), acct, key)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("models = %+v, want the 2 unique id-bearing entries", got)
	}
	if got[0].ID != "@cf/zai-org/glm-4.7-flash" || got[0].Display != "GLM-4.7 Flash" {
		t.Fatalf("first model = %+v", got[0])
	}
	if got[1].ID != "@cf/baai/bge-large-en-v1.5" {
		t.Fatalf("second model = %+v", got[1])
	}
}

// A success:false envelope must surface the provider's own words, not pass
// silently as an empty catalog (that is the bug class we are fixing).
func TestListModelsCloudflareRejectedEnvelope(t *testing.T) {
	c := &http.Client{Transport: cfCatalogTransport{key: "cfut_live_token"}}
	acct := testAccount(KindOpenAICompatible,
		"https://api.cloudflare.com/client/v4/accounts/9f3c2e1d4b5a69788796a5b4c3d2e1f0/ai/v1")
	a := NewOpenAICompatible(c)
	if _, err := a.ListModels(context.Background(), acct, "WRONG_KEY"); err == nil {
		t.Fatal("a rejected envelope must be an error, not an empty catalog")
	} else if !strings.Contains(err.Error(), "Invalid API Token") {
		t.Fatalf("error should quote the provider's words: %v", err)
	}
}
