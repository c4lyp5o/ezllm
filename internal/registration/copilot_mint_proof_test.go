package registration

// GitHub Copilot regression (2026-10-24): the device login succeeded, the
// catalog loaded through the minted token, and the key test STILL failed with
// `gpt-6.1-sol unavailable to this account (HTTP 400)` — because every model
// the account has is refused on /chat/completions.
//
// The credential was never in doubt: /models answered 200 through a token that
// only a valid GitHub credential can mint. But the tester had no way to record
// that proof, because it calibrates by re-probing with a bogus key — and
// Copilot refuses a bogus key while PREPARING the request (the mint 401s), so
// the calibration saw status 0 and read it as an outage. Inference then became
// the gate, and a model-shaped 400 failed a key that demonstrably worked.
//
// These tests drive the production adapter end to end with the network stubbed
// at the transport, so the mint, the catalog oracle, the calibration and the
// inference probe all run for real.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

const copilotRealKey = `{"access_token":"gho_real","refresh_token":"ghr_real"}`

// copilotTransport answers the three hosts a Copilot key test touches: the
// token mint (api.github.com), the catalog and the inference endpoints
// (api.githubcopilot.com). A fake or garbage key is refused AT THE MINT, the
// way the real service does it.
type copilotTransport struct {
	// chatStatus is what /chat/completions answers. Copilot answers 400
	// unsupported_api_for_model for every model that lives on /responses.
	chatStatus int
	mints      int
	chats      int
}

func (c *copilotTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	reply := func(status int, body string) (*http.Response, error) {
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: status, Header: h,
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/copilot_internal/v2/token"):
		c.mints++
		// Only the real GitHub token mints a Copilot session.
		if r.Header.Get("Authorization") != "token gho_real" {
			return reply(http.StatusUnauthorized, `{"message":"Bad credentials"}`)
		}
		exp := time.Now().Add(time.Hour).Unix()
		return reply(http.StatusOK, `{"token":"cop_tok","expires_at":`+itoa(exp)+`}`)
	case strings.HasSuffix(r.URL.Path, "/models"):
		if r.Header.Get("Authorization") != "Bearer cop_tok" {
			return reply(http.StatusUnauthorized, `{"message":"Bad credentials"}`)
		}
		return reply(http.StatusOK, `{"data":[{"id":"gpt-6.1-sol"},{"id":"gpt-5.3-codex"}]}`)
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		c.chats++
		if c.chatStatus == 0 {
			return reply(http.StatusOK, `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
		}
		return reply(c.chatStatus, `{"error":{"message":"model \"gpt-6.1-sol\" is not accessible via the /chat/completions endpoint","code":"unsupported_api_for_model"}}`)
	}
	return reply(http.StatusNotFound, `{}`)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func copilotTester(t *testing.T, tr *copilotTransport) (*Tester, *TestResult) {
	t.Helper()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	tester := NewTester(client, provider.NewRegistry(client), nil)
	acct := provider.Account{
		Name: "gh-copilot", Namespace: "copilot", Kind: provider.KindGitHubCopilot,
		BaseURL: "https://api.githubcopilot.com", Enabled: true,
	}
	return tester, tester.Run(context.Background(), acct, Options{
		Label:  "copilot",
		APIKey: copilotRealKey,
	})
}

// The bug: a working credential was failed by a model that lives on another
// wire. Now the mint's refusal of the CALIBRATION key counts as the proof the
// catalog 200 was, and generation is recorded as unverified instead of fatal.
func TestCopilotKeyTestAcceptsAWireMismatchOnceTheMintProvesTheKey(t *testing.T) {
	tr := &copilotTransport{chatStatus: http.StatusBadRequest}
	_, res := copilotTester(t, tr)

	if !res.OK {
		t.Fatalf("a proven Copilot credential must not fail on a wire mismatch; fail = %+v", res.Fail)
	}
	if res.authDeferred {
		t.Fatalf("the mint refusal is the calibration proof, so auth must not be deferred")
	}
	if tr.mints < 2 {
		t.Fatalf("expected the real mint and the calibration mint, got %d", tr.mints)
	}
	var proofed bool
	for _, s := range res.Steps {
		if s.Step == StepAuth && s.OK && strings.Contains(s.Detail, "before the wire") {
			proofed = true
		}
	}
	if !proofed {
		t.Fatalf("the auth step must say the fake credential was refused before the wire; steps = %+v", res.Steps)
	}
}

// The gate itself must reach the proof, not defer and hope inference fails
// into the second chance: a credential already proven should be recorded as
// proven, and the auth step is what the operator reads.
func TestCopilotAuthGateProvesTheKeyFromTheMintRefusal(t *testing.T) {
	tr := &copilotTransport{chatStatus: http.StatusBadRequest}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	adapter := provider.NewGitHubCopilot(client)
	tester := NewTester(client, provider.NewRegistry(client), nil)
	acct := provider.Account{
		Name: "gh-copilot", Namespace: "copilot", Kind: provider.KindGitHubCopilot,
		BaseURL: "https://api.githubcopilot.com", Enabled: true,
	}

	res := &TestResult{}
	if ok := tester.authGate(context.Background(), adapter, acct, copilotRealKey, &pacer{}, res); !ok {
		t.Fatalf("the oracle answered 200 for the real key; the gate must pass, fail = %+v", res.Fail)
	}
	if res.authDeferred {
		t.Fatalf("a mint that refuses the calibration key IS the proof; must not defer to inference")
	}
}

// The second chance, isolated: when the oracle cannot answer at all (a plan
// whose /models 404s) the gate defers, and the catalog calibration is what
// must recognise the mint's refusal. Without it, that account fails a key the
// mint accepted.
func TestCopilotCalibrateCatalogProvesFromTheMintRefusal(t *testing.T) {
	tr := &copilotTransport{chatStatus: http.StatusBadRequest}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	adapter := provider.NewGitHubCopilot(client)
	tester := NewTester(client, provider.NewRegistry(client), nil)
	acct := provider.Account{
		Name: "gh-copilot", Namespace: "copilot", Kind: provider.KindGitHubCopilot,
		BaseURL: "https://api.githubcopilot.com", Enabled: true,
	}

	res := &TestResult{authDeferred: true}
	proven, cerr := tester.calibrateCatalog(context.Background(), adapter, acct, &pacer{}, res)
	if !proven {
		t.Fatalf("the mint refuses a bogus credential, so the catalog calibration must prove the real key; cerr = %v", cerr)
	}
	if res.authDeferred {
		t.Fatalf("proof must clear authDeferred")
	}
}

// The control that keeps the fix honest: when the mint accepts EVERYTHING (a
// token exchange that does not discriminate), the calibration must not claim
// proof, so a generation failure still fails the key. Otherwise "the mint said
// no to garbage" would be satisfied by "the mint says yes to garbage".
func TestCopilotKeyTestStillFailsWhenTheMintAcceptsGarbage(t *testing.T) {
	tr := &permissiveMintTransport{}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	tester := NewTester(client, provider.NewRegistry(client), nil)
	acct := provider.Account{
		Name: "gh-copilot", Namespace: "copilot", Kind: provider.KindGitHubCopilot,
		BaseURL: "https://api.githubcopilot.com", Enabled: true,
	}
	res := tester.Run(context.Background(), acct, Options{Label: "copilot", APIKey: copilotRealKey})

	if res.OK {
		t.Fatalf("a mint that accepts a bogus key proves nothing; the key test must not pass")
	}
}

// permissiveMintTransport mints for ANY credential and refuses every model on
// /chat/completions: the shape of a provider whose oracle cannot discriminate.
type permissiveMintTransport struct{}

func (permissiveMintTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	reply := func(status int, body string) (*http.Response, error) {
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: status, Header: h,
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/copilot_internal/v2/token"):
		return reply(http.StatusOK, `{"token":"cop_tok","expires_at":`+itoa(time.Now().Add(time.Hour).Unix())+`}`)
	case strings.HasSuffix(r.URL.Path, "/models"):
		return reply(http.StatusOK, `{"data":[{"id":"gpt-6.1-sol"}]}`)
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		return reply(http.StatusBadRequest, `{"error":{"message":"not accessible via the /chat/completions endpoint","code":"unsupported_api_for_model"}}`)
	}
	return reply(http.StatusNotFound, `{}`)
}
