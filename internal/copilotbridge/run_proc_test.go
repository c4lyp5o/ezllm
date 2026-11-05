package copilotbridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run Runner against a real subprocess (a shell stub standing in
// for the CLI), so the parts parseStream tests cannot see are exercised: stdin
// delivery, argv shape, the timeout kill, typed exits and abort handling.

func stubCLI(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "copilot")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func within(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("did not return within %s", d)
	}
}

// The E2BIG case: a 600 KB prompt must arrive intact on stdin, and must not
// appear in argv (where one element dies at 128 KB).
func TestRunDeliversLargePromptOnStdinNotArgv(t *testing.T) {
	bin := stubCLI(t, `n=$(wc -c | tr -d ' ')
printf '{"type":"assistant.message","data":{"content":"bytes=%s args=%s"}}\n' "$n" "$*"`)
	r := &Runner{Bin: bin, Home: t.TempDir(), Timeout: 10 * time.Second}
	prompt := strings.Repeat("a", 600<<10)
	res, err := r.Run(context.Background(), "gpt-6.1-sol", prompt, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Text, "bytes=614400") {
		t.Errorf("stdin bytes wrong: %q", res.Text)
	}
	if !strings.Contains(res.Text, "-s --output-format=json --no-ask-user --model gpt-6.1-sol") {
		t.Errorf("argv shape wrong: %q", res.Text)
	}
	if strings.Contains(res.Text, "--allow-tool") {
		t.Error("--allow-tool must never be passed")
	}
}

func TestRunOmitsModelFlagForAuto(t *testing.T) {
	bin := stubCLI(t, `cat >/dev/null
printf '{"type":"assistant.message","data":{"content":"%s"}}\n' "$*"`)
	r := &Runner{Bin: bin, Timeout: 10 * time.Second}
	res, err := r.Run(context.Background(), "auto", "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "--model") {
		t.Errorf("auto must not pass --model: %q", res.Text)
	}
}

func TestRunClassifiesTypedExits(t *testing.T) {
	for stderr, want := range map[string]string{
		"Error: not logged in":                 "not_logged_in",
		"model x is not included in your plan": "model_not_in_plan",
		"premium request quota exceeded":       "quota_exhausted",
		"boom":                                 "cli_failed",
	} {
		bin := stubCLI(t, "cat >/dev/null\necho '"+stderr+"' >&2\nexit 1")
		r := &Runner{Bin: bin, Timeout: 10 * time.Second}
		_, err := r.Run(context.Background(), "", "hi", nil)
		var f *Failure
		if !errors.As(err, &f) || f.Code != want {
			t.Errorf("%q -> %v, want code %s", stderr, err, want)
		}
	}
}

func TestRunExitZeroWithoutMessageIsAnError(t *testing.T) {
	bin := stubCLI(t, "cat >/dev/null")
	r := &Runner{Bin: bin, Timeout: 10 * time.Second}
	if _, err := r.Run(context.Background(), "", "hi", nil); err == nil {
		t.Fatal("empty success accepted")
	}
}

func TestRunKillsAHungChildAtTimeout(t *testing.T) {
	bin := stubCLI(t, "exec sleep 30")
	r := &Runner{Bin: bin, Timeout: 300 * time.Millisecond}
	within(t, 8*time.Second, func() {
		_, err := r.Run(context.Background(), "", "hi", nil)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want deadline exceeded", err)
		}
	})
}

// Finding 2: a delta error (caller stopped reading) must kill the child at
// once, not wait out the timeout with a full stdout pipe.
func TestRunAbortsPromptlyWhenDeltaFails(t *testing.T) {
	bin := stubCLI(t, `cat >/dev/null
exec yes '{"type":"assistant.message_delta","data":{"deltaContent":"x"}}'`)
	r := &Runner{Bin: bin, Timeout: 60 * time.Second}
	boom := errors.New("client gone")
	within(t, 8*time.Second, func() {
		_, err := r.Run(context.Background(), "", "hi", func(string) error { return boom })
		if err == nil {
			t.Error("delta error swallowed")
		}
	})
}

func TestRunQueueHonoursContextWhileWaitingForASlot(t *testing.T) {
	bin := stubCLI(t, "cat >/dev/null\nexec sleep 30")
	r := &Runner{Bin: bin, Timeout: 20 * time.Second, MaxConcurrent: 1}
	hold, stopHold := context.WithCancel(context.Background())
	defer stopHold()                // kills the stub instead of orphaning `sleep 30`
	go r.Run(hold, "", "hold", nil) //nolint:errcheck
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	within(t, 3*time.Second, func() {
		if _, err := r.Run(ctx, "", "queued", nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestRunRejectsFastWhenTheQueueIsFull(t *testing.T) {
	bin := stubCLI(t, "cat >/dev/null\nexec sleep 30")
	r := &Runner{Bin: bin, Timeout: 20 * time.Second, MaxConcurrent: 1, MaxQueue: 1}
	hold, stop := context.WithCancel(context.Background())
	defer stop()
	go r.Run(hold, "", "running", nil) //nolint:errcheck
	time.Sleep(300 * time.Millisecond)
	go r.Run(hold, "", "waiting", nil) //nolint:errcheck
	time.Sleep(200 * time.Millisecond)
	within(t, 2*time.Second, func() {
		_, err := r.Run(context.Background(), "", "overflow", nil)
		var f *Failure
		if !errors.As(err, &f) || f.Code != "queue_full" {
			t.Errorf("err = %v, want queue_full", err)
		}
	})
	st := r.Stats()
	if st["rejected_queue_full"].(int64) != 1 || st["in_flight"].(int64) != 1 || st["queued"].(int64) != 1 {
		t.Errorf("stats = %v", st)
	}
}

func TestStatsCountOutcomesAndFirstByte(t *testing.T) {
	ok := stubCLI(t, `cat >/dev/null
echo '{"type":"assistant.message","data":{"content":"hi"}}'`)
	r := &Runner{Bin: ok, Timeout: 10 * time.Second}
	if _, err := r.Run(context.Background(), "", "x", nil); err != nil {
		t.Fatal(err)
	}
	r.Bin = stubCLI(t, "cat >/dev/null\necho 'not logged in' >&2\nexit 1")
	r.Run(context.Background(), "", "x", nil) //nolint:errcheck
	st := r.Stats()
	oc := st["outcomes"].(map[string]int64)
	if oc["ok"] != 1 || oc["not_logged_in"] != 1 || st["served"].(int64) != 2 {
		t.Errorf("stats = %v", st)
	}
	if r.firstByteN.Load() < 1 {
		t.Error("first-byte latency never recorded")
	}
}

// With the caller's tools emulated, the CLI's own tools must be invisible to
// the model; on plain chat the argv is unchanged.
func TestRunHidesBuiltinToolsOnlyWhenAsked(t *testing.T) {
	bin := stubCLI(t, `cat >/dev/null
printf '{"type":"assistant.message","data":{"content":"%s"}}\n' "$*"`)
	r := &Runner{Bin: bin, Timeout: 10 * time.Second}

	plain, err := r.Run(context.Background(), "auto", "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.Text, "--available-tools") || strings.Contains(plain.Text, "--disable-builtin-mcps") {
		t.Errorf("plain chat must not change: %q", plain.Text)
	}

	bare, err := r.Run(WithoutBuiltinTools(context.Background()), "auto", "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--available-tools=ezllm-no-builtin-tools", "--disable-builtin-mcps"} {
		if !strings.Contains(bare.Text, want) {
			t.Errorf("argv missing %s: %q", want, bare.Text)
		}
	}
	if strings.Contains(bare.Text, "--allow-tool") {
		t.Error("--allow-tool must never be passed")
	}
}
