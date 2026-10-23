package codexbridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake `codex app-server --stdio`. The child env
// is an allowlist, so the fake cannot be steered by an env var of its own: it
// reads its mode from $CODEX_HOME/mode and records what it was started with in
// $CODEX_HOME/started.json.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "app-server" && os.Args[2] == "--stdio" && os.Getenv("CODEX_HOME") != "" {
		fakeAppServer(os.Getenv("CODEX_HOME"))
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "exec" && os.Getenv("CODEX_HOME") != "" {
		fakeExecCLI(os.Getenv("CODEX_HOME"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeAppServer(home string) {
	mode := "ok"
	if b, err := os.ReadFile(filepath.Join(home, "mode")); err == nil {
		mode = strings.TrimSpace(string(b))
	}
	wd, _ := os.Getwd()
	started, _ := json.Marshal(map[string]any{"args": os.Args[1:], "env": os.Environ(), "pid": os.Getpid(), "cwd": wd})
	_ = os.WriteFile(filepath.Join(home, "started.json"), started, 0o600)

	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	notify := func(method string, params any) {
		send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue
		}
		reply := func(result any) { send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}) }
		switch m.Method {
		case "initialize":
			if mode == "protocol_broken" {
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "method not found: initialize"}})
				continue
			}
			reply(map[string]any{"userAgent": "fake", "codexHome": home})
		case "model/list":
			reply(map[string]any{"data": []any{
				map[string]any{"id": "gpt-6.1-sol", "displayName": "GPT-6.1 Sol", "hidden": false, "isDefault": true,
					"defaultReasoningEffort": "low", "supportedReasoningEfforts": []any{
						map[string]any{"reasoningEffort": "low", "description": ""}, map[string]any{"reasoningEffort": "high", "description": ""}}},
				map[string]any{"id": "gpt-reserve", "displayName": "Reserve", "hidden": true, "isDefault": false,
					"defaultReasoningEffort": "medium", "supportedReasoningEfforts": []any{}},
			}, "nextCursor": nil})
		case "account/rateLimits/read":
			reply(map[string]any{"rateLimits": map[string]any{"planType": "plus",
				"primary":   map[string]any{"usedPercent": 1, "windowDurationMins": 300, "resetsAt": 1792739595},
				"secondary": map[string]any{"usedPercent": 12.5, "windowDurationMins": 10080, "resetsAt": 1793326395}}})
		case "thread/start":
			_ = os.WriteFile(filepath.Join(home, "thread_start.json"), m.Params, 0o600)
			if mode == "bad_model" {
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32600, "message": "model gpt-nope is not supported"}})
				continue
			}
			reply(map[string]any{"thread": map[string]any{"id": "thr-1"}})
		case "turn/start":
			_ = os.WriteFile(filepath.Join(home, "turn_start.json"), m.Params, 0o600)
			reply(map[string]any{"turn": map[string]any{"id": "turn-1", "status": "inProgress", "items": []any{}}})
			switch mode {
			case "hang":
				for i := 0; ; i++ {
					notify("item/agentMessage/delta", map[string]any{"itemId": "m1", "delta": fmt.Sprint(i), "threadId": "thr-1", "turnId": "turn-1"})
					time.Sleep(5 * time.Millisecond)
				}
			case "exit_midturn":
				notify("item/agentMessage/delta", map[string]any{"itemId": "m1", "delta": "par", "threadId": "thr-1", "turnId": "turn-1"})
				os.Exit(3)
			case "usage_limit":
				notify("error", map[string]any{"willRetry": false, "threadId": "thr-1", "turnId": "turn-1",
					"error": map[string]any{"message": "You've hit your usage limit.", "codexErrorInfo": "usageLimitExceeded"}})
				notify("turn/completed", map[string]any{"threadId": "thr-1", "turn": map[string]any{"id": "turn-1", "status": "failed", "items": []any{},
					"error": map[string]any{"message": "You've hit your usage limit.", "codexErrorInfo": "usageLimitExceeded"}}})
			default:
				for _, d := range []string{"po", "ng"} {
					notify("item/agentMessage/delta", map[string]any{"itemId": "m1", "delta": d, "threadId": "thr-1", "turnId": "turn-1"})
				}
				notify("thread/tokenUsage/updated", map[string]any{"threadId": "thr-1", "turnId": "turn-1", "tokenUsage": map[string]any{
					"total": map[string]any{"totalTokens": 5098, "inputTokens": 5093, "cachedInputTokens": 4992, "outputTokens": 5, "reasoningOutputTokens": 2},
					"last":  map[string]any{"totalTokens": 5098, "inputTokens": 5093, "cachedInputTokens": 4992, "outputTokens": 5, "reasoningOutputTokens": 2}}})
				notify("turn/completed", map[string]any{"threadId": "thr-1", "turn": map[string]any{"id": "turn-1", "status": "completed", "items": []any{}}})
			}
		default:
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "fake: " + m.Method}})
		}
	}
}

// fakeRunner is a Runner whose binary is this test binary in the given mode.
func fakeRunner(t *testing.T, mode string) (*Runner, string) {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "mode"), []byte(mode), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Runner{Bin: os.Args[0], CodexHome: home, Timeout: 20 * time.Second, Version: "test"}, home
}

func readStarted(t *testing.T, home string) (args, env []string, pid int, cwd string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "started.json"))
	if err != nil {
		t.Fatalf("fake app-server never started: %v", err)
	}
	var s struct {
		Args []string `json:"args"`
		Env  []string `json:"env"`
		Pid  int      `json:"pid"`
		Cwd  string   `json:"cwd"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s.Args, s.Env, s.Pid, s.Cwd
}

// liveCLI returns the real Codex CLI and its isolated login, or skips. Set
// EZLLM_CODEX_LIVE_BIN / EZLLM_CODEX_LIVE_HOME to point elsewhere.
func liveCLI(t *testing.T) (bin, home string) {
	t.Helper()
	bin = os.Getenv("EZLLM_CODEX_LIVE_BIN")
	home = os.Getenv("EZLLM_CODEX_LIVE_HOME")
	if bin == "" {
		if p, err := exec.LookPath("codex"); err == nil {
			bin = p
		}
	}
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".codex-sub")
		}
	}
	if bin == "" {
		t.Skip("no codex CLI on this host")
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
		t.Skip("no isolated Codex login at " + home)
	}
	return bin, home
}

// fakeExecCLI mimics `codex exec --json` (event shapes measured 2026-10-23).
func fakeExecCLI(home string) {
	mode := "ok"
	if b, err := os.ReadFile(filepath.Join(home, "mode")); err == nil {
		mode = strings.TrimSpace(string(b))
	}
	stdin, _ := io.ReadAll(os.Stdin)
	rec, _ := json.Marshal(map[string]any{"args": os.Args[1:], "stdin": string(stdin)})
	_ = os.WriteFile(filepath.Join(home, "exec.json"), rec, 0o600)
	fmt.Println(`{"type":"thread.started","thread_id":"t"}`)
	fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Code Mode is unavailable because code-mode host is disabled."}}`)
	fmt.Println(`{"type":"turn.started"}`)
	if mode == "exec_400" {
		fmt.Println(`{"type":"turn.failed","error":{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"The 'gpt-nope' model is not supported when using Codex with a ChatGPT account.\"}}"}}`)
		return
	}
	fmt.Println(`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"Bonjour !"}}`)
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":2999,"cached_input_tokens":2176,"cache_write_input_tokens":0,"output_tokens":6,"reasoning_output_tokens":0}}`)
}
