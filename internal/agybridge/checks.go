package agybridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// LiveTools proves the opt-in adapter using the same request parser, envelope
// validator and stateless history replay as HTTP. It executes no caller tool:
// the gate supplies a fresh synthetic result and checks it is consumed.
// Answers, arguments and result values are never included in errors.
func LiveTools(ctx context.Context, ex Executor, model string) error {
	return liveTools(ctx, ex, model, "none")
}
func liveTools(ctx context.Context, ex Executor, model, followupChoice string) error {
	tools := json.RawMessage(`[{"type":"function","function":{"name":"bridge_lookup","description":"Read a value from the caller","parameters":{"type":"object","properties":{"key":{"type":"string"}},"required":["key"],"additionalProperties":false}}}]`)
	prompt := "Use the caller's bridge_lookup tool for key probe. After the caller returns a value, repeat that exact value as your final answer. Do not guess or use native tools."
	messages := []any{map[string]any{"role": "user", "content": prompt}}
	run := func(choice string) (Result, *toolAdapter, error) {
		body, _ := json.Marshal(map[string]any{"model": model, "tools": tools, "tool_choice": choice, "parallel_tool_calls": false, "messages": messages})
		req, err := parseRequest(body, true)
		if err != nil {
			return Result{}, nil, err
		}
		res, err := ex.Run(ctx, req, nil)
		return res, req.Adapter, err
	}
	first, a, err := run("auto")
	if err != nil {
		return err
	}
	_, calls, err := a.decode(first)
	if err != nil || len(calls) != 1 || calls[0].Function.Name != "bridge_lookup" {
		return errors.New("tool selection did not return one valid caller lookup")
	}
	if first.Usage.InputTokens+first.Usage.CacheRead <= 0 || first.Usage.OutputTokens <= 0 {
		return errors.New("tool turn reported no usage")
	}
	nonce := "bridge_probe_" + randomHex(12)
	messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}, map[string]any{"role": "tool", "tool_call_id": calls[0].ID, "content": nonce})
	second, a, err := run(followupChoice)
	if err != nil {
		return fmt.Errorf("tool result replay: %w", err)
	}
	text, calls, err := a.decode(second)
	if err != nil || len(calls) != 0 || !strings.Contains(text, nonce) {
		return errors.New("tool result replay did not consume the caller result")
	}
	if second.Usage.InputTokens+second.Usage.CacheRead <= 0 || second.Usage.OutputTokens <= 0 {
		return errors.New("tool result replay reported no usage")
	}
	return nil
}

// LiveConversationCache verifies caller-tool result replay actually resumes a
// native conversation. Tiny probes need not meet the provider's cache threshold.
func LiveConversationCache(ctx context.Context, ex interface {
	Executor
	CacheStats() map[string]any
}, model string) error {
	before := ex.CacheStats()
	hits, ok := before["hits"].(uint64)
	if before["enabled"] != true || !ok {
		return errors.New("conversation cache mode is not enabled")
	}
	if err := liveTools(ctx, ex, model, "auto"); err != nil {
		return err
	}
	after, ok := ex.CacheStats()["hits"].(uint64)
	if !ok || after != hits+1 {
		return errors.New("caller result replay did not resume the native conversation")
	}
	return nil
}
