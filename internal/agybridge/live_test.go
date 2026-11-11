package agybridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Live checks against the real CLI and the operator's Antigravity login. A turn
// spends plan quota, so they run only with EZLLM_AGY_LIVE=1, against
// EZLLM_AGY_BIN (default ~/.local/share/agy-bridge/<qualified>/agy) and
// EZLLM_AGY_HOME (default ~/.agy-sub).
func liveCLI(t *testing.T) (bin, home string) {
	t.Helper()
	if os.Getenv("EZLLM_AGY_LIVE") != "1" {
		t.Skip("set EZLLM_AGY_LIVE=1 to spend plan quota on live checks")
	}
	userHome, _ := os.UserHomeDir()
	bin = os.Getenv("EZLLM_AGY_BIN")
	if bin == "" {
		bin = userHome + "/.local/share/agy-bridge/" + QualifiedVersion + "/agy"
	}
	home = os.Getenv("EZLLM_AGY_HOME")
	if home == "" {
		home = userHome + "/.agy-sub"
	}
	return bin, home
}

func TestLivePreflight(t *testing.T) {
	bin, home := liveCLI(t)
	p, err := RunPreflight(context.Background(), bin, home)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Qualified || len(p.Models) == 0 {
		t.Fatalf("preflight %+v", p)
	}
	t.Logf("agy %s, %d models", p.Version, len(p.Models))
}

// TestLiveTurn passes on an eligible login, and on one Google has not yet
// cleared it must fail as 403 account_ineligible with the sign-in link withheld.
func TestLiveTurn(t *testing.T) {
	bin, home := liveCLI(t)
	model := os.Getenv("EZLLM_AGY_MODEL")
	if model == "" {
		model = "gemini-3.8-flash-low"
	}
	r := &Runner{Bin: bin, Home: home, Timeout: 120 * time.Second}
	var deltas int
	start := time.Now()
	res, err := r.Run(context.Background(), Request{Model: model, Prompt: "Reply with exactly: pong"},
		func(string) error { deltas++; return nil })
	if err != nil {
		ne := asNative(err)
		if ne.Code == "account_ineligible" && !strings.Contains(ne.Message, "https://") {
			t.Skipf("login not yet eligible (403 as designed, link withheld): %s", ne.Message)
		}
		t.Fatalf("%+v", ne)
	}
	if !strings.Contains(strings.ToLower(res.Text), "pong") || deltas == 0 || res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
		t.Fatalf("text=%q deltas=%d usage=%+v", res.Text, deltas, res.Usage)
	}
	t.Logf("wall %s, %d deltas, usage %+v, tool steps %d", time.Since(start).Round(time.Millisecond), deltas, res.Usage, res.ToolSteps)
}

// TestLiveHTTP exercises the shipped HTTP surface with the actual subscription.
func TestLiveHTTP(t *testing.T) {
	bin, home := liveCLI(t)
	model := os.Getenv("EZLLM_AGY_MODEL")
	if model == "" {
		model = "gemini-3.8-flash-low"
	}
	runner := &Runner{Bin: bin, Home: home, Timeout: 120 * time.Second}
	srv := httptest.NewServer((&Server{Runner: runner, Token: "live-fixture", Models: []Model{{ID: model}}}).Handler())
	defer srv.Close()
	for _, stream := range []bool{false, true} {
		body, _ := json.Marshal(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "Reply with exactly: pong"}}, "stream": stream, "stream_options": map[string]any{"include_usage": true}})
		ctx, cancel := context.WithTimeout(context.Background(), 130*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer live-fixture")
		began := time.Now()
		resp, err := srv.Client().Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if readErr != nil || resp.StatusCode != 200 {
			t.Fatalf("stream=%v status=%d read_error=%v", stream, resp.StatusCode, readErr)
		}
		if stream {
			var answer strings.Builder
			input, output := 0, 0
			finished, done := false, false
			for _, line := range strings.Split(string(data), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				payload := strings.TrimPrefix(line, "data: ")
				if payload == "[DONE]" {
					done = true
					continue
				}
				var event struct {
					Choices []struct {
						Delta        struct{ Content string }
						FinishReason string `json:"finish_reason"`
					}
					Usage struct {
						Input  int `json:"prompt_tokens"`
						Output int `json:"completion_tokens"`
					}
				}
				if json.Unmarshal([]byte(payload), &event) != nil {
					t.Fatal("invalid stream event")
				}
				for _, choice := range event.Choices {
					answer.WriteString(choice.Delta.Content)
					finished = finished || choice.FinishReason == "stop"
				}
				if event.Usage.Input > 0 {
					input, output = event.Usage.Input, event.Usage.Output
				}
			}
			if !done || !finished || !strings.Contains(answer.String(), "pong") || input <= 0 || output <= 0 {
				t.Fatal("stream answer, usage or termination invalid")
			}
		} else {
			var v struct {
				Choices []struct{ Message struct{ Content string } }
				Usage   struct {
					Input  int `json:"prompt_tokens"`
					Output int `json:"completion_tokens"`
				}
			}
			if json.Unmarshal(data, &v) != nil || len(v.Choices) != 1 || !strings.Contains(v.Choices[0].Message.Content, "pong") || v.Usage.Input <= 0 || v.Usage.Output <= 0 {
				t.Fatal("non-stream answer or usage invalid")
			}
		}
		t.Logf("stream=%v wall=%s answer/usage/terminal checks passed", stream, time.Since(began).Round(time.Millisecond))
	}
}
