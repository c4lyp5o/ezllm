package registration

// GitHub Copilot regression (2026-10-23): the key test picked gpt-5.3-codex,
// probed /chat/completions, and Copilot answered 400 "model ... is not
// accessible via the /chat/completions endpoint" — codex models live on
// /responses. That one model-shaped 400 failed a credential the auth oracle
// had already proven. Two rules pin the fix:
//   - an endpoint-mismatch refusal is about the MODEL: try the next candidate;
//   - once a proof exists, a non-auth generation failure is recorded as
//     unverified, not fatal. Without a proof it stays fatal.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

const mismatchValidKey = "sk-valid-mismatch-key"

// fakeMismatch: an openai-compatible upstream whose catalog authenticates
// (401 for a wrong key) unless publicCatalog, and whose chat endpoint refuses
// every model in refused with Copilot's exact wording.
func fakeMismatch(t *testing.T, publicCatalog bool, refused ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		valid := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == mismatchValidKey
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			if !valid && !publicCatalog {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":{"message":"bad credentials"}}`))
				return
			}
			w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.3-codex-mini"},{"id":"gpt-4.1"}]}`))
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			if !valid {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var p struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(body, &p)
			for _, m := range refused {
				if p.Model == m {
					w.WriteHeader(http.StatusBadRequest)
					w.Write([]byte(`{"error":{"message":"model \"` + m + `\" is not accessible via the /chat/completions endpoint","code":"unsupported_api_for_model"}}`))
					return
				}
			}
			w.Write([]byte(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runMismatch(t *testing.T, srv *httptest.Server) *TestResult {
	t.Helper()
	client := srv.Client()
	tester := NewTester(client, provider.NewRegistry(client), nil)
	res := tester.Run(context.Background(), oaiAccount(srv.URL), Options{
		Label:  "mismatch",
		APIKey: mismatchValidKey,
	})
	return res
}

func TestEndpointMismatchAdvancesToNextCandidate(t *testing.T) {
	res := runMismatch(t, fakeMismatch(t, false, "gpt-5.3-codex-mini"))
	if res.Fail != nil {
		t.Fatalf("endpoint mismatch failed the key: %+v", res.Fail)
	}
	joined := stepDetails(res.Steps)
	if !strings.Contains(joined, "gpt-5.3-codex-mini unavailable — falling back to gpt-4.1") {
		t.Fatalf("probe did not advance past the mismatched model: %s", joined)
	}
	if d, _ := stepDetail(res, StepInference); !strings.HasPrefix(d, "gpt-4.1 ·") {
		t.Fatalf("inference verdict = %q, want a gpt-4.1 generation", d)
	}
}

func TestModelRefusalsDoNotBlockAProvenKey(t *testing.T) {
	res := runMismatch(t, fakeMismatch(t, false, "gpt-5.3-codex-mini", "gpt-4.1"))
	if res.Fail != nil {
		t.Fatalf("a proven key was failed by model refusals: %+v", res.Fail)
	}
	d, _ := stepDetail(res, StepInference)
	if !strings.Contains(d, "generation unverified") || !strings.Contains(d, "HTTP 400") {
		t.Fatalf("inference verdict = %q, want an unverified note naming the 400", d)
	}
	if strings.Contains(d, "capacity") {
		t.Fatalf("a 400 must not be described as capacity: %q", d)
	}
}

func TestModelRefusalsWithoutProofStillFail(t *testing.T) {
	res := runMismatch(t, fakeMismatch(t, true, "gpt-5.3-codex-mini", "gpt-4.1"))
	if res.Fail == nil || res.OK {
		t.Fatalf("no proof + every model refused must not store the key; detail=%q", res.Detail)
	}
}
