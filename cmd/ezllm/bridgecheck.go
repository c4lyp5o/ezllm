package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/agybridge"
	"github.com/c4lyp5o/ezllm/internal/claudebridge"
	"github.com/c4lyp5o/ezllm/internal/codexbridge"
	"github.com/c4lyp5o/ezllm/internal/server"
)

// Bridge CLI qualification and the bridge-check gate (SPEC-bridge-update.md
// §4.2–4.3). The host updater runs `ezllm bridge-check` against a staged CLI
// before it promotes it; boot runs the same checks, so the updater cannot pass
// a release boot would reject. The gate is stricter than boot: flag drift, the
// exec fallback or a lost code-mode host are reported and served by boot, but
// refuse a promotion.

// qualifiedFile is the updater's record of versions that passed the live proof.
// It sits in the bridge directory, next to the version folders and `current`.
const qualifiedFile = "qualified.json"

type qualifiedRecord struct {
	Versions map[string]json.RawMessage `json:"versions"`
}

// recordQualifies reports whether version is listed in the qualified.json
// nearest the binary: its own directory, then up to two parents — the bridge
// root for both `current/claude` and `current/bin/codex`. The walk is lexical,
// so a `current` symlink leads to the bridge root, not into the version folder.
func recordQualifies(bin, version string) (bool, error) {
	dir := filepath.Dir(bin)
	for i := 0; i < 3; i++ {
		path := filepath.Join(dir, qualifiedFile)
		b, err := os.ReadFile(path)
		if err == nil {
			var rec qualifiedRecord
			if err := json.Unmarshal(b, &rec); err != nil {
				return false, fmt.Errorf("%s: %w", path, err)
			}
			_, ok := rec.Versions[version]
			return ok, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return false, nil
}

// releaseState is the `cli_release` a bridge reports: qualified when the
// version is the one this build was measured on or the updater proved it.
func releaseState(bin, version, measured string) string {
	if version == measured {
		return "qualified"
	}
	ok, err := recordQualifies(bin, version)
	switch {
	case err != nil:
		return "unqualified (" + qualifiedFile + " unreadable: " + err.Error() + ")"
	case ok:
		return "qualified (live proof recorded by the updater)"
	}
	return "unqualified (measured on " + measured + "; re-run the live proof)"
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// bridgeVerdict is what `ezllm bridge-check --json` prints for the updater.
type bridgeVerdict struct {
	Bridge   string   `json:"bridge"`
	OK       bool     `json:"ok"`
	Version  string   `json:"version,omitempty"`
	Release  string   `json:"release,omitempty"`
	Live     bool     `json:"live"`
	Problems []string `json:"problems"`
}

var errBridgeCheckFailed = errors.New("bridge-check: the CLI did not pass")

func runBridgeCheck(args []string, stdout io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: ezllm bridge-check {claude|codex|agy} [--bin PATH] [--home PATH] [--live [--model ID] [--tool-model ID]] [--json]")
	}
	name := args[0]
	set := flag.NewFlagSet("bridge-check "+name, flag.ContinueOnError)
	bin := set.String("bin", "", "CLI binary (default: the bridge's EZLLM_*_BIN or its default)")
	home := set.String("home", "", "login directory (default: the bridge's login env or its default)")
	asJSON := set.Bool("json", false, "print the verdict as JSON")
	live := set.Bool("live", false, "after the static checks pass, drive the bridge through real turns (spends plan quota)")
	model := set.String("model", "", "model for the plain live turn (default: the bridge's smallest)")
	toolModel := set.String("tool-model", "", "model for the client-tool live turn (claude, codex)")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}

	ctx := context.Background()
	var v bridgeVerdict
	var liveRun func() []string
	switch name {
	case "claude":
		b, d := or(*bin, envOrDefault(envBridgeBin, "/opt/claude/claude")), or(*home, envOrDefault(envBridgeDir, "/claude-sub"))
		v = checkClaudeCLI(b, d)
		liveRun = func() []string {
			return liveClaude(ctx, b, d, or(*model, "claude-haiku-5-5"), or(*toolModel, "claude-haiku-5-5"))
		}
	case "codex":
		b, h := or(*bin, envOrDefault(envCodexBin, "/opt/codex/bin/codex")), or(*home, envOrDefault(envCodexHome, "/codex-sub"))
		var pre codexbridge.Preflight
		v, pre = checkCodexCLI(ctx, b, h)
		liveRun = func() []string {
			return liveCodex(ctx, b, h, pre, or(*model, "gpt-5.6-luna"), or(*toolModel, "gpt-6.1-sol"))
		}
	case "agy":
		b, h := or(*bin, envOrDefault(envAgyBin, "/opt/agy/agy")), or(*home, envOrDefault(envAgyHome, "/agy-sub"))
		v = checkAgyCLI(ctx, b, h)
		liveRun = func() []string { return liveAgy(ctx, b, h, or(*model, "gemini-3.8-flash-low")) }
	default:
		return fmt.Errorf("bridge-check: unknown bridge %q (claude, codex or agy)", name)
	}
	// Live turns spend quota and prove nothing a static refusal has not already
	// decided, so they run only on a CLI that passed every static check.
	if *live && len(v.Problems) == 0 {
		v.Live = true
		v.Problems = liveRun()
	}
	v.OK = len(v.Problems) == 0
	if v.Problems == nil {
		v.Problems = []string{}
	}
	if *asJSON {
		b, _ := json.Marshal(v)
		fmt.Fprintln(stdout, string(b))
	} else {
		fmt.Fprintf(stdout, "%s %s: %s\n", v.Bridge, v.Version, v.Release)
		for _, p := range v.Problems {
			fmt.Fprintln(stdout, "  problem:", p)
		}
	}
	if !v.OK {
		return errBridgeCheckFailed
	}
	return nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func checkClaudeCLI(bin, dir string) bridgeVerdict {
	v := bridgeVerdict{Bridge: "claude"}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		v.Problems = append(v.Problems, "binary does not resolve: "+err.Error())
		return v
	}
	raw, drift := claudeCLICheck(resolved)
	if raw == "unavailable" {
		v.Problems = append(v.Problems, drift)
		return v
	}
	v.Version = claudeVersion(raw)
	v.Release = releaseState(bin, v.Version, claudeQualifiedVersion)
	if drift != "" {
		v.Problems = append(v.Problems, "flags the CLI no longer accepts: "+drift)
	}
	if state, plan := claudeLoginCheck(resolved, dir); state != "logged-in" {
		v.Problems = append(v.Problems, "login "+state+": "+plan)
	}
	return v
}

func checkCodexCLI(ctx context.Context, bin, home string) (bridgeVerdict, codexbridge.Preflight) {
	v := bridgeVerdict{Bridge: "codex"}
	var pre codexbridge.Preflight
	if runtime.GOOS != "linux" {
		v.Problems = append(v.Problems, "the codex bridge is a Linux feature (this build is "+runtime.GOOS+")")
		return v, pre
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		v.Problems = append(v.Problems, "binary does not resolve: "+err.Error())
		return v, pre
	}
	pre, err = codexbridge.RunPreflight(ctx, resolved, home)
	v.Version = pre.Version
	if pre.Version != "" {
		v.Release = releaseState(bin, pre.Version, codexbridge.QualifiedVersion)
	}
	if err != nil {
		v.Problems = append(v.Problems, err.Error())
		return v, pre
	}
	if pre.Channel == codexbridge.ChannelExec {
		v.Problems = append(v.Problems, "app-server lacks "+strings.Join(pre.MissingRPCs, ", ")+": the bridge would fall back to exec")
	}
	if host := codexbridge.CodeModeHost(resolved); !fileExists(host) {
		v.Problems = append(v.Problems, "code-mode host missing ("+host+"): client tools would be off")
	}
	return v, pre
}

func checkAgyCLI(ctx context.Context, bin, home string) bridgeVerdict {
	v := bridgeVerdict{Bridge: "agy"}
	if runtime.GOOS != "linux" {
		v.Problems = append(v.Problems, "the agy bridge is a Linux feature (this build is "+runtime.GOOS+")")
		return v
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		v.Problems = append(v.Problems, "binary does not resolve: "+err.Error())
		return v
	}
	pre, err := agybridge.RunPreflight(ctx, resolved, home)
	v.Version = pre.Version
	if pre.Version != "" {
		v.Release = releaseState(bin, pre.Version, agybridge.QualifiedVersion)
	}
	if err != nil {
		v.Problems = append(v.Problems, err.Error())
		return v
	}
	if len(pre.Models) == 0 {
		v.Problems = append(v.Problems, "the CLI listed no models")
	}
	return v
}

// Live turns (--live). Each drives the bridge's own Runner in-process — no
// HTTP, no gateway, no ledger — through the paths a CLI release most often
// breaks: a plain turn and, where the bridge serves them, a client-tool turn.
// The model's text is judged, never printed.

const liveTurnTimeout = 120 * time.Second

const livePrompt = "Reply with exactly: pong"

var liveWeatherSchema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`

// judgeTurn is the verdict for one live turn: empty when it passed. Usage is
// required of an answer only: a Codex segment that stops on a tool call
// reports 0 tokens by design — they land on the follow-up (codex design §2).
func judgeTurn(turn string, err error, text string, outputTokens int64, wantTool, gotTool string) string {
	switch {
	case err != nil:
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		return "live " + turn + ": " + msg
	case wantTool == "" && outputTokens <= 0:
		return "live " + turn + ": no output tokens reported"
	case wantTool != "" && gotTool != wantTool:
		return "live " + turn + ": no " + wantTool + " tool call came back (got " + or(gotTool, "none") + ")"
	case wantTool == "" && !strings.Contains(strings.ToLower(text), "pong"):
		return "live " + turn + ": the answer did not follow the prompt"
	}
	return ""
}

func appendIf(problems []string, p string) []string {
	if p != "" {
		problems = append(problems, p)
	}
	return problems
}

func liveClaude(ctx context.Context, bin, dir, model, toolModel string) []string {
	r := &claudebridge.Runner{Bin: bin, ConfigDir: dir, Timeout: liveTurnTimeout, MaxConcurrent: 1}
	turn := func(req claudebridge.Request) (claudebridge.Result, string, string, error) {
		alias, err := claudebridge.ModelAlias(req.Model)
		if err != nil {
			return claudebridge.Result{}, "", "", err
		}
		var text, tool strings.Builder
		res, err := r.Run(ctx, req, alias, func(f claudebridge.Frame) error {
			var ev struct {
				ContentBlock struct{ Type, Name string } `json:"content_block"`
				Delta        struct{ Text string }       `json:"delta"`
			}
			if json.Unmarshal(f.Event, &ev) == nil {
				text.WriteString(ev.Delta.Text)
				if ev.ContentBlock.Type == "tool_use" && tool.Len() == 0 {
					tool.WriteString(ev.ContentBlock.Name)
				}
			}
			return nil
		})
		if err == nil && res.NativeError != "" {
			err = errors.New(res.NativeError)
		}
		return res, text.String(), tool.String(), err
	}
	user := func(s string) []claudebridge.Message {
		b, _ := json.Marshal(s)
		return []claudebridge.Message{{Role: "user", Content: b}}
	}

	var problems []string
	res, text, _, err := turn(claudebridge.Request{Model: model, MaxTokens: 64, Stream: true, Messages: user(livePrompt)})
	problems = appendIf(problems, judgeTurn("plain turn", err, text, int64(res.Usage.OutputTokens), "", ""))
	tools := `[{"name":"get_weather","description":"Get the current weather for a city","input_schema":` + liveWeatherSchema + `}]`
	res, _, tool, err := turn(claudebridge.Request{Model: toolModel, MaxTokens: 1024, Stream: true, Tools: json.RawMessage(tools),
		Messages: user("What's the weather in Paris right now? Use the tool.")})
	return appendIf(problems, judgeTurn("tool turn", err, "", int64(res.Usage.OutputTokens), "get_weather", tool))
}

func liveCodex(ctx context.Context, bin, home string, pre codexbridge.Preflight, model, toolModel string) []string {
	helper, err := os.Executable()
	if err != nil {
		return []string{"live: cannot locate this binary for the Landlock helper: " + err.Error()}
	}
	tools := decideCodexTools(helper, bin, strings.EqualFold(strings.TrimSpace(os.Getenv(envCodexToolsNoLL)), "on"))
	r := &codexbridge.Runner{Bin: bin, CodexHome: home, Timeout: liveTurnTimeout, MaxConcurrent: 1,
		Version: server.Version, Channel: pre.Channel, Sandbox: tools.sandbox}
	// The tool turn parks its child for a follow-up that never comes.
	defer r.Shutdown()

	var problems []string
	res, err := r.Run(ctx, codexbridge.Request{Model: model, Instructions: codexbridge.DefaultInstructions,
		Input: []codexbridge.UserInput{{Type: "text", Text: livePrompt}}}, nil)
	problems = appendIf(problems, judgeTurn("plain turn", err, res.Text, res.Usage.OutputTokens, "", ""))
	// A host without Landlock serves no client tools in production either: the
	// tool turn would test a path this bridge does not offer here.
	if tools.off != "" {
		return problems
	}
	res, err = r.Run(ctx, codexbridge.Request{Model: toolModel, Instructions: codexbridge.DefaultInstructions + " Use the provided tools when they help.",
		Input: []codexbridge.UserInput{{Type: "text", Text: "What's the weather in Kuala Lumpur? Use get_weather, then answer in one sentence."}},
		Tools: []codexbridge.ToolSpec{{Name: "get_weather", Description: "Get the current weather for a city", Parameters: json.RawMessage(liveWeatherSchema)}}}, nil)
	var got string
	if len(res.ToolCalls) > 0 {
		got = res.ToolCalls[0].Name
	}
	return appendIf(problems, judgeTurn("tool turn", err, "", res.Usage.OutputTokens, "get_weather", got))
}

func liveAgy(ctx context.Context, bin, home, model string) []string {
	r := &agybridge.Runner{Bin: bin, Home: home, Timeout: liveTurnTimeout, MaxConcurrent: 1}
	res, err := r.Run(ctx, agybridge.Request{Model: model, Prompt: livePrompt}, func(string) error { return nil })
	return appendIf(nil, judgeTurn("plain turn", err, res.Text, int64(res.Usage.OutputTokens), "", ""))
}
