package copilotbridge

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestToolBatchDeliveredBeforeAbortAndDelete(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "delivery failure"}[fail], func(t *testing.T) {
			f := &fakeCLI{events: []string{evBatch}, aborted: []string{evUsage, evIdle}}
			turn := channelTurn()
			calls := 0
			deliveryErr := errors.New("client disconnected")
			turn.OnTools = func(batch Result) error {
				calls++
				if f.called("session.abort") || f.called("session.delete") {
					t.Error("batch delivered after cleanup")
				}
				if len(batch.ToolCalls) != 2 {
					t.Errorf("batch=%+v", batch.ToolCalls)
				}
				if fail {
					return deliveryErr
				}
				return nil
			}
			res, err := f.drive(t, turn, nil)
			if fail && !errors.Is(err, deliveryErr) || !fail && err != nil {
				t.Fatalf("err=%v", err)
			}
			if calls != 1 || !f.called("session.abort") || !f.called("session.delete") || res.Usage.PromptTokens != 100 {
				t.Fatalf("calls=%d usage=%+v cleanup=%v/%v", calls, res.Usage, f.called("session.abort"), f.called("session.delete"))
			}
		})
	}
}

type cleanupExec struct {
	fakeExec
	release <-chan struct{}
}

func (e cleanupExec) RunChannel(ctx context.Context, turn ChannelTurn, _ func(string) error) (Result, error) {
	res := Result{ToolCalls: []ToolCall{{ID: "call_early", Name: "get_weather", Arguments: `{"city":"Paris"}`}}}
	if turn.OnTools != nil {
		if err := turn.OnTools(res); err != nil {
			return res, err
		}
	}
	select {
	case <-e.release:
	case <-ctx.Done():
		return res, ctx.Err()
	}
	res.Usage = Usage{PromptTokens: 123, CompletionTokens: 7, PremiumRequests: 1}
	return res, nil
}

func TestHTTPToolBatchPrecedesCleanupButUsageAndDoneFollowIt(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	s := &Server{Runner: cleanupExec{release: release}, Token: "t", Models: []Model{{ID: "auto"}}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}],"stream":true,"stream_options":{"include_usage":true}}`))
	req.Header.Set("Authorization", "Bearer t")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	var early strings.Builder
	for !strings.Contains(early.String(), "call_early") {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		early.WriteString(line)
	}
	if strings.Contains(early.String(), "[DONE]") || strings.Contains(early.String(), "prompt_tokens") {
		t.Fatal("completion or usage before cleanup")
	}
	unblock()
	tail, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	full := early.String() + string(tail)
	if strings.Count(full, "call_early") != 1 || !strings.Contains(string(tail), `"prompt_tokens":123`) || !strings.Contains(string(tail), `"finish_reason":"tool_calls"`) || !strings.Contains(string(tail), "[DONE]") {
		t.Fatalf("bad stream: %s", full)
	}
}

func TestStageTimingsAreAggregatedAndSnapshotsAreIndependent(t *testing.T) {
	r := &Runner{}
	r.observeStage("queue", 2*time.Millisecond)
	r.observeStage("queue", 4*time.Millisecond)
	stats := r.Stats()["stage_timings"].(map[string]any)
	q := stats["queue"].(map[string]any)
	if q["count"] != int64(2) || q["avg_ms"] != float64(3) || q["last_ms"] != float64(4) {
		t.Fatalf("queue timing=%v", q)
	}
	q["count"] = int64(100)
	if r.Stats()["stage_timings"].(map[string]any)["queue"].(map[string]any)["count"] != int64(2) {
		t.Fatal("stats exposed mutable counters")
	}
}
