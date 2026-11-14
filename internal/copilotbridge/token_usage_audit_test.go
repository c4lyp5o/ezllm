package copilotbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The measured fixture has inclusive per-model inputTokens, matching the
// aggregate input+cache counters. Losing tokenDetails must not count cache twice.
func TestUsageFallbackDoesNotDoubleCountCachedInput(t *testing.T) {
	var ev cliEvent
	raw := `{"type":"session.usage_checkpoint","data":{"accountingSnapshot":{"providerModelMetrics":[{"modelId":"gpt-6.1-sol","metrics":{"usage":{"inputTokens":13963,"outputTokens":7,"cacheReadTokens":0,"cacheWriteTokens":13960,"reasoningTokens":3}}}]}}}`
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	var u Usage
	ev.applyUsage(&u)
	if u.PromptTokens != 13963 || u.CachedWrite != 13960 || u.CompletionTokens != 7 || u.ReasoningTokens != 3 {
		t.Fatalf("usage=%+v", u)
	}
}

func TestUsageKeepsReasoningSubsetWithPrimaryTokenCounters(t *testing.T) {
	var ev cliEvent
	raw := `{"type":"session.usage_checkpoint","data":{"accountingSnapshot":{"tokenDetails":{"input":{"tokenCount":10},"cache_read":{"tokenCount":90},"output":{"tokenCount":20}},"providerModelMetrics":[{"modelId":"m","metrics":{"usage":{"inputTokens":100,"outputTokens":20,"cacheReadTokens":90,"reasoningTokens":15}}}]}}}`
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	var u Usage
	ev.applyUsage(&u)
	if u.PromptTokens != 100 || u.CompletionTokens != 20 || u.CachedRead != 90 || u.ReasoningTokens != 15 {
		t.Fatalf("usage=%+v", u)
	}
	// Snapshots replace counters, rather than being billed again at every event.
	ev.applyUsage(&u)
	if u.PromptTokens != 100 || u.CompletionTokens != 20 || u.ReasoningTokens != 15 {
		t.Fatalf("checkpoint counted twice: %+v", u)
	}
}

func TestUsageFallbackIncludesEveryReportedModel(t *testing.T) {
	var ev cliEvent
	raw := `{"type":"session.usage_checkpoint","data":{"accountingSnapshot":{"providerModelMetrics":[{"modelId":"a","metrics":{"usage":{"inputTokens":100,"outputTokens":20,"cacheReadTokens":60,"reasoningTokens":8}}},{"modelId":"b","metrics":{"usage":{"inputTokens":200,"outputTokens":30,"cacheReadTokens":70,"cacheWriteTokens":10,"reasoningTokens":9}}}]}}}`
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	var u Usage
	ev.applyUsage(&u)
	if u.PromptTokens != 300 || u.CachedRead != 130 || u.CachedWrite != 10 || u.CompletionTokens != 50 || u.ReasoningTokens != 17 {
		t.Fatalf("usage=%+v", u)
	}
}

type alreadyStartedExec struct {
	plain     int
	usage     Usage
	emit      bool
	preflight bool
}

func (f *alreadyStartedExec) Run(context.Context, string, string, func(string) error) (Result, error) {
	f.plain++
	return Result{Text: "second generation"}, nil
}
func (f *alreadyStartedExec) RunChannel(_ context.Context, _ ChannelTurn, delta func(string) error) (Result, error) {
	if f.emit && delta != nil {
		_ = delta("first generation")
	}
	err := error(&Failure{Code: "chat_tools_unsupported", Msg: "undeclared tool after inference"})
	if f.preflight {
		err = toolsRefused("channel unavailable", nil)
	}
	return Result{Usage: f.usage}, err
}

func TestPlainChatNeverFallsBackAfterModelWork(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name            string
			usage           Usage
			emit, preflight bool
		}{
			{name: "post-inference rejection"},
			{name: "known usage", usage: Usage{PromptTokens: 100, CompletionTokens: 3}, preflight: true},
			{name: "known premium charge", usage: Usage{PremiumRequests: 1}, preflight: true},
			{name: "known credit charge", usage: Usage{NanoAIU: 100}, preflight: true},
			{name: "emitted output", emit: true, preflight: true},
		} {
			if tc.emit && !stream {
				continue
			}
			t.Run(tc.name+map[bool]string{false: "/json", true: "/sse"}[stream], func(t *testing.T) {
				f := &alreadyStartedExec{usage: tc.usage, emit: tc.emit, preflight: tc.preflight}
				s := &Server{Runner: f, Token: "t", Models: []Model{{ID: "auto"}}}
				suffix := "false"
				if stream {
					suffix = "true"
				}
				rec := postChat(s, `{"model":"auto","stream":`+suffix+`,"messages":[{"role":"user","content":"hello"}]}`)
				if f.plain != 0 || strings.Contains(rec.Body.String(), "second generation") {
					t.Fatalf("performed another generation: plain=%d status=%d", f.plain, rec.Code)
				}
				if !tc.emit && rec.Code != http.StatusBadRequest {
					t.Fatalf("status=%d", rec.Code)
				}
			})
		}
	}
}

func TestWireUsagePreservesNativeCacheWrites(t *testing.T) {
	// CLI 1.0.105's live Hermes-shaped probe: three cold tokens and 7,405
	// cache-write tokens, with no cache read. The ledger must retain that split.
	u := openAIUsage(Usage{PromptTokens: 7408, CompletionTokens: 15, CachedWrite: 7405, ReasoningTokens: 8})
	d := u["prompt_tokens_details"].(map[string]any)
	if d["cache_write_tokens"] != 7405 {
		t.Fatalf("native cache writes lost on wire: %v", u)
	}
	if u["prompt_tokens"] != 7408 {
		t.Fatalf("inclusive input changed: %v", u)
	}
}
