package codexbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Explicitly gated: this bounded synthetic caller-tool task spends subscription
// quota, writes only t.TempDir(), and never executes model-supplied shell text.
func TestLiveCallerTaskFidelity(t *testing.T) {
	if os.Getenv("EZLLM_CODEX_FIDELITY_LIVE") != "1" {
		t.Skip("explicit live fidelity experiment only")
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("the live fidelity test requires bun on PATH")
	}
	requireLandlock(t)
	bin, home := liveCLI(t)
	for _, cache := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cache), func(t *testing.T) {
			dir := t.TempDir()
			r := &Runner{Bin: bin, CodexHome: home, Timeout: 90 * time.Second, ForkCache: cache, NativeHistory: os.Getenv("EZLLM_CODEX_FIDELITY_NATIVE") == "1", SandboxMode: SandboxWritable, Sandbox: &Sandbox{Helper: os.Args[0]}}
			defer r.Shutdown()
			schema := func(s string) any {
				var v any
				if err := json.Unmarshal([]byte(s), &v); err != nil {
					t.Fatal(err)
				}
				return v
			}
			tool := func(name, desc, s string) any {
				return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": desc, "parameters": schema(s)}}
			}
			task := `Finish a tiny runnable todo web app using Bun built-ins only; no dependencies. Create exactly these six files in the caller workspace: package.json (type module, test script bun test), backend.js (export async function handle(request) returning a Response: GET /healthz returns {status:"ok"}; GET /items returns a JSON array initially empty; POST /items with JSON {name:"..."} stores an item and returns status 201), backend.test.js (bun:test coverage of these routes), index.html, app.js (browser UI that lists and creates items using fetch), README.md (run/test instructions). Then run bun test using terminal. Complete implementation and verification before giving a final answer. Tools use paths relative to the caller workspace. Do not run a server or install packages.`
			messages := []any{
				map[string]any{"role": "system", "content": "You are Hermes, a coding agent. Use caller tools to implement authorized work and verify it. Be accurate about results."},
				map[string]any{"role": "user", "content": task},
				map[string]any{"role": "assistant", "content": "Checking frontend dependencies.", "tool_calls": []any{map[string]any{"id": "old-install", "type": "function", "function": map[string]any{"name": "terminal", "arguments": `{"command":"bun add -d vite"}`}}}},
				map[string]any{"role": "tool", "tool_call_id": "old-install", "content": `{"exit_code":-1,"status":"error","error":"This foreground command appears to start a long-lived server/watch process. Run it with background=true."}`},
				map[string]any{"role": "assistant", "content": "I have not built the app yet. The install was blocked. Nothing is ready to use."},
				map[string]any{"role": "user", "content": "Continue and finish the app. No dependencies are needed; use Bun built-ins as requested."},
			}
			tools := []any{tool("write_file", "Write a complete file in the caller workspace.", `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`), tool("read_file", "Read a caller workspace file.", `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`), tool("terminal", "Run verification. Only the exact command bun test is supported.", `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			batches, calls, totalIn, totalOut := 0, 0, int64(0), int64(0)
			tested := false
			finalChars := 0
			for ; batches < 16; batches++ {
				body, _ := json.Marshal(map[string]any{"model": "gpt-6.1-terra", "messages": messages, "tools": tools, "parallel_tool_calls": false})
				req, err := ParseRequest(body)
				if err != nil {
					t.Fatal(err)
				}
				res, err := r.Run(ctx, req, nil)
				if err != nil {
					t.Fatal(err)
				}
				totalIn += res.Usage.InputTokens
				totalOut += res.Usage.OutputTokens
				if len(res.ToolCalls) == 0 {
					finalChars = len(res.Text)
					break
				}
				assistant := map[string]any{"role": "assistant", "content": res.Text, "tool_calls": openAIToolCalls(res.ToolCalls, false)}
				messages = append(messages, assistant)
				for _, call := range res.ToolCalls {
					calls++
					var a struct{ Path, Content, Command string }
					if err := json.Unmarshal([]byte(call.Arguments), &a); err != nil {
						t.Fatal(err)
					}
					output := ""
					safe := filepath.IsLocal(a.Path)
					switch call.Name {
					case "write_file":
						if !safe {
							output = "error: path must be relative to caller workspace"
							break
						}
						p := filepath.Join(dir, a.Path)
						if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(p, []byte(a.Content), 0600); err != nil {
							t.Fatal(err)
						}
						output = "File written."
					case "read_file":
						if !safe {
							output = "error: path must be relative to caller workspace"
							break
						}
						b, e := os.ReadFile(filepath.Join(dir, a.Path))
						if e != nil {
							output = "File not found."
						} else {
							output = string(b)
						}
					case "terminal":
						if a.Command != "bun test" {
							output = "error: only bun test is supported; no server or dependencies are needed"
							break
						}
						cmd := exec.CommandContext(ctx, bun, "test")
						cmd.Dir = dir
						b, e := cmd.CombinedOutput()
						output = string(b)
						tested = e == nil
						if e != nil {
							output += "\nCommand failed."
						}
					default:
						output = "error: unknown tool"
					}
					messages = append(messages, map[string]any{"role": "tool", "tool_call_id": call.ID, "content": output})
				}
			}
			files := 0
			for _, p := range []string{"package.json", "backend.js", "backend.test.js", "index.html", "app.js", "README.md"} {
				if b, e := os.ReadFile(filepath.Join(dir, p)); e == nil && len(b) > 0 {
					files++
				}
			}
			// Independent route checks prevent a model-authored empty test from passing.
			check := exec.CommandContext(ctx, bun, "--eval", `import {handle} from './backend.js';const get=async p=>handle(new Request('http://test'+p));if((await (await get('/healthz')).json()).status!=='ok')throw Error('health');let a=await(await get('/items')).json();if(!Array.isArray(a)||a.length)throw Error('initial');let r=await handle(new Request('http://test/items',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:'proof'})}));if(r.status!==201)throw Error('create');a=await(await get('/items')).json();if(a.length!==1||a[0].name!=='proof')throw Error('list');`)
			check.Dir = dir
			_, checkErr := check.CombinedOutput()
			if cache && files == 6 && tested && checkErr == nil {
				probe := append([]any(nil), messages[:6]...)
				probe[5] = map[string]any{"role": "user", "content": "Reply with exactly pong. Do not use tools."}
				body, _ := json.Marshal(map[string]any{"model": "gpt-6.1-terra", "messages": probe, "tools": tools, "parallel_tool_calls": false})
				req, err := ParseRequest(body)
				if err != nil {
					t.Fatal(err)
				}
				got, err := r.Run(ctx, req, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got.Text != "pong" || len(got.ToolCalls) > 0 || r.CacheHits() != 1 {
					t.Fatal("fresh caller request did not reuse a clean template")
				}
			}
			report := map[string]any{"cache": cache, "native_history": r.NativeHistory, "tool_batches": batches, "tool_calls": calls, "required_files": files, "model_tests_passed": tested, "independent_routes_passed": checkErr == nil, "final_chars": finalChars, "input_tokens": totalIn, "output_tokens": totalOut, "cache_hits": r.CacheHits()}
			b, _ := json.Marshal(report)
			t.Log(string(b))
			if out := os.Getenv("EZLLM_CODEX_FIDELITY_OUTPUT"); out != "" {
				if err := os.WriteFile(fmt.Sprintf("%s-cache-%t.json", out, cache), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if files != 6 || !tested || checkErr != nil {
				t.Error("caller task incomplete; see bounded experiment metrics")
			}
		})
	}
}
