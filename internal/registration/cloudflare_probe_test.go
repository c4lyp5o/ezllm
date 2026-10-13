package registration

// Cloudflare Workers AI key-test behaviour, measured live 2026-10-13 (the
// sibling of the NVIDIA NIM fix — same symptom, different cause):
//
//   - The generic openai-compatible oracle GET {base}/usage does not exist
//     there (405), and Cloudflare's real credential check lives OUTSIDE the
//     account base URL: GET /client/v4/user/tokens/verify — 200 + "active" on
//     a real token, 401 "Invalid API Token" on a well-formed cfut_ fake, but
//     400 "Invalid format for Authorization header" on a malformed fake. The
//     calibration therefore needs the adapter's format-valid CalibrationKey.
//   - GET {base}/models is 405 ("GET not supported for requested URI") —
//     Cloudflare's OpenAI-compat prefix has no model list. The catalog comes
//     from the native envelope {"success":true,"result":[...]} which the
//     OpenAI parser reads as ZERO models — exactly the failure Calypso hit:
//     "no credential-checking endpoint and no model to probe with".

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

type cfTransport struct{ key string }

func cfJSON(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}
}

func (tr cfTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	auth := req.Header.Get("Authorization")
	switch {
	case strings.HasSuffix(req.URL.Path, "/user/tokens/verify"):
		// The oracle — absolute URL, account-independent.
		if auth == "Bearer "+tr.key {
			return cfJSON(req, 200, `{"result":{"id":"7b2a92ae09c4f95af5ae037bee652a6e","status":"active"},"success":true,"errors":[],"messages":[{"code":10000,"message":"This API Token is valid and active","type":null}]}`), nil
		}
		if strings.HasPrefix(auth, "Bearer cfut_") && len(auth) == len("Bearer cfut_")+40 {
			return cfJSON(req, 401, `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}],"messages":[],"result":null}`), nil
		}
		// Malformed token shape — what the generic fakeKey() produced.
		return cfJSON(req, 400, `{"success":false,"errors":[{"code":6003,"message":"Invalid request headers","error_chain":[{"code":6111,"message":"Invalid format for Authorization header"}]}],"messages":[],"result":null}`), nil

	case strings.HasSuffix(req.URL.Path, "/ai/models/search"):
		if auth != "Bearer "+tr.key {
			return cfJSON(req, 401, `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}],"messages":[],"result":null}`), nil
		}
		return cfJSON(req, 200, `{"errors":[],"messages":[],"success":true,"result":[
				{"id":"uuid-001","name":"@cf/zai-org/glm-4.7-flash","description":"GLM-4.7 Flash"},
				{"id":"uuid-002","name":"@cf/meta/llama-3.3-70b-instruct-fp8-fast","description":"Llama 3.3 70B Instruct (fast)"},
				{"id":"uuid-003","name":"@cf/zai-org/glm-4.7-flash","description":"duplicate must be skipped"}
			]}`), nil

	case strings.HasSuffix(req.URL.Path, "/ai/v1/models"):
		// The OpenAI-shaped path Cloudflare does NOT implement.
		return cfJSON(req, 405, `{"result":null,"success":false,"errors":[{"code":7001,"message":"GET not supported for requested URI."}],"messages":[]}`), nil

	case strings.HasSuffix(req.URL.Path, "/chat/completions"):
		if auth != "Bearer "+tr.key {
			return cfJSON(req, 401, `{"error":{"message":"Invalid API Token"}}`), nil
		}
		return cfJSON(req, 200, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`), nil

	case strings.HasSuffix(req.URL.Path, "/usage"):
		// Not the oracle on Cloudflare.
		return cfJSON(req, 405, `{"result":null,"success":false,"errors":[{"code":7001,"message":"GET not supported for requested URI."}],"messages":[]}`), nil
	}
	return cfJSON(req, 404, `{"success":false,"errors":[{"code":7000,"message":"not found"}],"messages":[],"result":null}`), nil
}

func TestCloudflareKeyTestEndToEnd(t *testing.T) {
	const key = "cfut_real_live_token_value"
	client := &http.Client{Transport: cfTransport{key: key}}
	registry := provider.NewRegistry(client)
	tester := NewTester(client, registry, nil)
	acct := provider.Account{
		Kind:      provider.KindOpenAICompatible,
		BaseURL:   "https://api.cloudflare.com/client/v4/accounts/9f3c2e1d4b5a69788796a5b4c3d2e1f0/ai/v1",
		Namespace: "cloudflare-workers-ai",
	}

	res := tester.Run(context.Background(), acct, Options{APIKey: key, Label: "default"})
	joined := stepDetails(res.Steps)
	if !res.OK {
		t.Fatalf("the Cloudflare key test must pass: fail=%+v steps=%s", res.Fail, joined)
	}

	// Oracle: the absolute tokens/verify URL was used AND calibration rejected
	// the format-valid cfut_ fake (a malformed sk- fake would 400 → "inconclusive"
	// → deferred → this exact line never appears).
	if !strings.Contains(joined, "oracle 200, calibration rejected the fake key") {
		t.Fatalf("host-scoped oracle or CalibrationKey broken (must not defer): %s", joined)
	}
	if !strings.Contains(joined, "no quota endpoint") {
		t.Fatalf("quota step should record Cloudflare's missing endpoint: %s", joined)
	}

	// Catalog: the native envelope parsed — NOT the 405 /models path, NOT an
	// empty list (the old behaviour that produced Calypso's failure).
	if len(res.Catalog) != 2 {
		t.Fatalf("catalog = %+v, want the 2 unique @cf models from the envelope", res.Catalog)
	}
	if res.Catalog[0].ID != "@cf/zai-org/glm-4.7-flash" || res.Catalog[1].ID != "@cf/meta/llama-3.3-70b-instruct-fp8-fast" {
		t.Fatalf("catalog ids = %+v, want @cf/zai-org/glm-4.7-flash + @cf/meta/llama-3.3-70b-instruct-fp8-fast", res.Catalog)
	}
	if res.Catalog[0].Display != "GLM-4.7 Flash" {
		t.Fatalf("display name not carried from the envelope: %+v", res.Catalog[0])
	}

	// Inference still runs (oracle + generation, belt and braces).
	if !strings.Contains(joined, "@cf/zai-org/glm-4.7-flash") || !strings.Contains(joined, "·") {
		t.Fatalf("inference step missing the probe candidate: %s", joined)
	}
}

// A non-Cloudflare openai-compatible account must keep the generic /usage
// oracle and no calibration-key override — the host-scoped branch is inert.
func TestCloudflareOracleDoesNotLeakToOtherAccounts(t *testing.T) {
	a := provider.NewOpenAICompatible(&http.Client{})
	acct := provider.Account{Kind: provider.KindOpenAICompatible, BaseURL: "https://api.openai.com/v1"}
	hosted, ok := a.(provider.HostedAuthOracle)
	if !ok {
		t.Fatalf("openai-compatible must implement HostedAuthOracle")
	}
	o := hosted.AuthOracleFor(acct)
	if o.Path != "/usage" || o.CalibrationKey != "" {
		t.Fatalf("non-Cloudflare oracle = %+v, want relative /usage and no CalibrationKey", o)
	}
}
