package agybridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type proofExecutor struct {
	n           int
	failReplay  bool
	omitUsage   bool
	fullyCached bool
}

func (f *proofExecutor) Run(_ context.Context, req Request, _ func(string) error) (Result, error) {
	f.n++
	if req.Adapter == nil {
		return Result{}, errors.New("missing adapter")
	}
	text := `{"content":null,"tool_calls":[{"name":"bridge_lookup","arguments":{"key":"probe"}}]}`
	if f.n == 2 {
		transcript := strings.Split(req.Prompt, "Caller transcript (JSON):\n")
		var messages []struct {
			Role, Content string
			ToolCallID    string `json:"tool_call_id"`
		}
		if len(transcript) != 2 || json.Unmarshal([]byte(transcript[1]), &messages) != nil {
			return Result{}, errors.New("missing transcript")
		}
		last := messages[len(messages)-1]
		if last.Role != "tool" || last.ToolCallID == "" {
			return Result{}, errors.New("missing correlated result")
		}
		value := last.Content
		if f.failReplay {
			value = "unrelated answer"
		}
		raw, _ := json.Marshal(map[string]any{"content": value, "tool_calls": []any{}})
		text = string(raw)
	}
	usage := Usage{InputTokens: 1, OutputTokens: 1}
	if f.fullyCached {
		usage.InputTokens = 0
		usage.CacheRead = 10
	}
	if f.omitUsage {
		usage = Usage{}
	}
	return Result{Text: text, Usage: usage}, nil
}

func TestLiveToolsProvesSelectionAndCallerResultReplay(t *testing.T) {
	f := &proofExecutor{}
	if err := LiveTools(context.Background(), f, "m"); err != nil {
		t.Fatal(err)
	}
	if f.n != 2 {
		t.Fatalf("turns=%d", f.n)
	}
}
func TestLiveToolsRefusesMissingResultAndUsage(t *testing.T) {
	for _, f := range []*proofExecutor{{failReplay: true}, {omitUsage: true}} {
		err := LiveTools(context.Background(), f, "m")
		if err == nil || strings.Contains(err.Error(), "bridge_probe_") {
			t.Fatalf("invalid proof outcome: %v", err)
		}
	}
}

func TestLiveToolsAcceptsFullyCachedInput(t *testing.T) {
	f := &proofExecutor{fullyCached: true}
	if err := LiveTools(context.Background(), f, "m"); err != nil {
		t.Fatal(err)
	}
}

type cacheProof struct {
	proofExecutor
	disabled, noHits bool
}

func (f *cacheProof) CacheStats() map[string]any {
	hits := uint64(0)
	if f.n >= 2 && !f.noHits {
		hits = 1
	}
	return map[string]any{"enabled": !f.disabled, "hits": hits}
}
func TestLiveConversationCacheRequiresActualReuseProof(t *testing.T) {
	if err := LiveConversationCache(context.Background(), &cacheProof{}, "m"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []*cacheProof{{disabled: true}, {noHits: true}} {
		if err := LiveConversationCache(context.Background(), bad, "m"); err == nil {
			t.Fatal("cache mode qualified without reuse")
		}
	}
}
