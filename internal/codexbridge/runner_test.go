package codexbridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func simpleRequest() Request {
	return Request{Model: "gpt-6.1-sol", Instructions: DefaultInstructions,
		Input: []UserInput{{Type: "text", Text: "Reply with exactly: pong"}}}
}

func TestRunStreamsDeltasAndReturnsTheTurnUsage(t *testing.T) {
	r, home := fakeRunner(t, "ok")
	var deltas []string
	res, err := r.Run(context.Background(), simpleRequest(), func(d Delta) error {
		deltas = append(deltas, d.Content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "pong" || strings.Join(deltas, "|") != "po|ng" {
		t.Fatalf("text=%q deltas=%v", res.Text, deltas)
	}
	if res.Usage != (Usage{InputTokens: 5093, CachedTokens: 4992, OutputTokens: 5, ReasoningTokens: 2}) {
		t.Fatalf("usage = %+v (want tokenUsage.total)", res.Usage)
	}

	var ts map[string]any
	b, _ := os.ReadFile(filepath.Join(home, "thread_start.json"))
	_ = json.Unmarshal(b, &ts)
	if ts["sandbox"] != "read-only" || ts["approvalPolicy"] != "never" || ts["ephemeral"] != true ||
		ts["baseInstructions"] != DefaultInstructions || ts["model"] != "gpt-6.1-sol" {
		t.Fatalf("thread/start params = %v", ts)
	}
}

// Every lockdown flag reaches the child: the no-execution proof (§2.5) holds
// only for exactly this argv.
func TestChildArgvCarriesTheLockdown(t *testing.T) {
	r, home := fakeRunner(t, "ok")
	if _, err := r.Run(context.Background(), simpleRequest(), nil); err != nil {
		t.Fatal(err)
	}
	args, _, _, cwd := readStarted(t, home)
	if len(args) < 2 || args[0] != "app-server" || args[1] != "--stdio" {
		t.Fatalf("argv = %v, want app-server --stdio first", args)
	}
	for _, f := range append(slices.Clone(LockdownFeatures), "code_mode_host", "shell_tool", "in_app_updates") {
		i := slices.Index(args, f)
		if i < 1 || args[i-1] != "--disable" {
			t.Errorf("feature %q is not disabled in argv %v", f, args)
		}
	}
	if !slices.Contains(args, "tools.web_search=false") {
		t.Errorf("web search is not disabled: %v", args)
	}
	if !strings.Contains(cwd, "codex-bridge-") {
		t.Errorf("child cwd = %q, want a private temp dir", cwd)
	}
}

// The child env is an allowlist: ezllm's secrets never reach the CLI.
func TestChildEnvIsAllowlisted(t *testing.T) {
	t.Setenv("EZLLM_MASTER_KEY", "super-secret")
	t.Setenv("OPENAI_API_KEY", "sk-should-not-pass")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("LANG", "C.UTF-8")
	r, home := fakeRunner(t, "ok")
	if _, err := r.Run(context.Background(), simpleRequest(), nil); err != nil {
		t.Fatal(err)
	}
	_, env, _, _ := readStarted(t, home)
	joined := strings.Join(env, "\n")
	for _, leaked := range []string{"super-secret", "sk-should-not-pass", "aws-secret"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("secret %q reached the child env", leaked)
		}
	}
	if !slices.Contains(env, "CODEX_HOME="+home) || !slices.Contains(env, "LANG=C.UTF-8") {
		t.Errorf("env is missing CODEX_HOME or LANG: %v", env)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") && !strings.Contains(kv, "codex-bridge-") {
			t.Errorf("HOME = %q, want the per-request temp dir", kv)
		}
	}
}

func TestPlanLimitIs429WithTheCLIsWords(t *testing.T) {
	r, _ := fakeRunner(t, "usage_limit")
	_, err := r.Run(context.Background(), simpleRequest(), nil)
	var ne *NativeError
	if !errors.As(err, &ne) || ne.Status != 429 || !strings.Contains(ne.Message, "usage limit") {
		t.Fatalf("err = %v, want 429 carrying the CLI's message", err)
	}
}

func TestModelRefusedByThreadStartIs400(t *testing.T) {
	r, _ := fakeRunner(t, "bad_model")
	_, err := r.Run(context.Background(), simpleRequest(), nil)
	var ne *NativeError
	if !errors.As(err, &ne) || ne.Status != 400 || !strings.Contains(ne.Message, "not supported") {
		t.Fatalf("err = %v, want 400 with the CLI's words", err)
	}
}

func TestChildExitMidTurnIs502(t *testing.T) {
	r, _ := fakeRunner(t, "exit_midturn")
	_, err := r.Run(context.Background(), simpleRequest(), nil)
	var ne *NativeError
	if !errors.As(err, &ne) || ne.Status != 502 {
		t.Fatalf("err = %v, want 502 native_closed", err)
	}
}

// A cancelled request (client gone) must kill and reap the child and remove
// its work dir: a leaked child keeps drawing on the plan.
func TestCancelMidStreamReapsTheChild(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc to observe the child")
	}
	r, home := fakeRunner(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	got := 0
	_, err := r.Run(ctx, simpleRequest(), func(Delta) error {
		got++
		if got == 3 {
			cancel()
		}
		return nil
	})
	if err == nil {
		t.Fatal("Run returned nil after cancel")
	}
	_, _, pid, cwd := readStarted(t, home)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d still exists after cancel", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Dir(cwd)); !os.IsNotExist(err) {
		t.Fatalf("work dir %s survived the run", filepath.Dir(cwd))
	}
}

func TestSinkErrorEndsTheRun(t *testing.T) {
	r, _ := fakeRunner(t, "hang")
	gone := errors.New("client went away")
	_, err := r.Run(context.Background(), simpleRequest(), func(Delta) error { return gone })
	if !errors.Is(err, gone) {
		t.Fatalf("err = %v, want the sink error", err)
	}
}

func TestModelsAndRateLimitsReads(t *testing.T) {
	r, _ := fakeRunner(t, "ok")
	models, err := r.Models(context.Background())
	if err != nil || len(models) != 2 || models[1].ID != "gpt-reserve" || !models[1].Hidden ||
		!slices.Equal(models[0].Efforts, []string{"low", "high"}) {
		t.Fatalf("models = %+v err = %v (hidden models must be included)", models, err)
	}
	rl, err := r.RateLimits(context.Background())
	if err != nil || rl.Primary == nil || rl.Primary.WindowDurationMins != 300 || rl.Secondary.UsedPercent != 12.5 {
		t.Fatalf("rate limits = %+v err = %v", rl, err)
	}
}

func TestMissingBinaryIs503(t *testing.T) {
	r := &Runner{Bin: filepath.Join(t.TempDir(), "no-codex"), CodexHome: t.TempDir()}
	_, err := r.Run(context.Background(), simpleRequest(), nil)
	var ne *NativeError
	if !errors.As(err, &ne) || ne.Status != 503 || ne.Code != "binary_missing" {
		t.Fatalf("err = %v, want 503 binary_missing", err)
	}
}
