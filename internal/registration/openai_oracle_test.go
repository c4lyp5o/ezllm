package registration

// Regression: adding api.openai.com/v1 with a VALID key failed with
// "could not reach the credential endpoint". Root cause: the openai-compatible
// auth oracle was GET /usage (an ssn-gpt-specific path). OpenAI answers
// /v1/usage with 400 "Missing required parameter: 'date'" even for a good key,
// and 400 is not an auth reject for this adapter, so the gate fell through to
// the default branch. Fake below mirrors the live OpenAI responses recorded
// 2026-10-10 (models 200/401; usage 400 for a valid key, 401 for a bogus one).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

const oaiValidKey = "sk-proj-valid-test-key"

func fakeOpenAI(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		valid := bearer == oaiValidKey
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			if !valid {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`))
				return
			}
			w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5-2025-08-07","object":"model","owned_by":"system"}]}`))
		case strings.HasSuffix(r.URL.Path, "/usage"):
			if !valid {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`))
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"Missing required parameter: 'date'.","type":"invalid_request_error","param":"date","code":"missing_required_parameter"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"message":"unknown path"}}`))
		}
	}))
}

func oaiAccount(baseURL string) provider.Account {
	return provider.Account{
		ID:      1,
		Name:    "openai",
		Kind:    provider.KindOpenAICompatible,
		BaseURL: baseURL + "/v1",
		Enabled: true,
	}
}

// A valid key must pass the auth gate. Before the fix this fails with
// "could not reach the credential endpoint" at step auth.
func TestOpenAIValidKeyPassesAuthGate(t *testing.T) {
	srv := fakeOpenAI(t)
	defer srv.Close()

	client := srv.Client()
	tester := NewTester(client, provider.NewRegistry(client), nil)
	res := tester.Run(context.Background(), oaiAccount(srv.URL), Options{
		Label:          "primary",
		APIKey:         oaiValidKey,
		SkipInference:  true,
		ProtocolInline: true,
	})

	if res.Fail != nil {
		t.Fatalf("valid OpenAI key rejected: step=%s msg=%q upstream=%d",
			res.Fail.Step, res.Fail.Message, res.Fail.UpstreamStatus)
	}
	if !res.OK {
		t.Fatalf("expected OK, got steps=%+v", res.Steps)
	}
}

// A bogus key must still be rejected, with a real auth message, not the
// misleading "could not reach" one.
func TestOpenAIBogusKeyRejectedAsAuth(t *testing.T) {
	srv := fakeOpenAI(t)
	defer srv.Close()

	client := srv.Client()
	tester := NewTester(client, provider.NewRegistry(client), nil)
	res := tester.Run(context.Background(), oaiAccount(srv.URL), Options{
		Label:          "primary",
		APIKey:         "sk-proj-bogus",
		SkipInference:  true,
		ProtocolInline: true,
	})

	if res.OK {
		t.Fatalf("bogus key accepted: steps=%+v", res.Steps)
	}
	if res.Fail == nil {
		t.Fatalf("expected a failure, got none")
	}
	if strings.Contains(res.Fail.Message, "could not reach") {
		t.Fatalf("bogus key reported as unreachable: %q", res.Fail.Message)
	}
}
