package codexbridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func readExec(t *testing.T, home string) (args []string, stdin string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "exec.json"))
	if err != nil {
		t.Fatalf("exec channel never ran: %v", err)
	}
	var rec struct {
		Args  []string `json:"args"`
		Stdin string   `json:"stdin"`
	}
	_ = json.Unmarshal(b, &rec)
	return rec.Args, rec.Stdin
}

func TestExecChannelRunsWithTheSameLockdown(t *testing.T) {
	r, home := fakeRunner(t, "ok")
	r.Channel = ChannelExec
	req := simpleRequest()
	req.Effort = "high"
	var chunks []string
	res, err := r.Run(context.Background(), req, func(d Delta) error {
		if d.Content != "" {
			chunks = append(chunks, d.Content)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Bonjour !" || len(chunks) != 1 || res.Usage.InputTokens != 2999 || res.Usage.CachedTokens != 2176 {
		t.Fatalf("res=%+v chunks=%v (exec emits the message whole)", res, chunks)
	}
	args, stdin := readExec(t, home)
	for _, want := range []string{"--json", "--ephemeral", "--ignore-user-config", "--ignore-rules", "read-only", "shell_tool", "code_mode_host",
		`instructions="You are a helpful assistant."`, `model_reasoning_effort="high"`} {
		if !slices.Contains(args, want) {
			t.Errorf("exec argv lacks %q: %v", want, args)
		}
	}
	if stdin != "Reply with exactly: pong" {
		t.Errorf("prompt on stdin = %q", stdin)
	}
}

func TestExecFailureKeepsTheUpstreamStatus(t *testing.T) {
	r, _ := fakeRunner(t, "exec_400")
	r.Channel = ChannelExec
	_, err := r.Run(context.Background(), simpleRequest(), nil)
	var ne *NativeError
	if !errors.As(err, &ne) || ne.Status != 400 || !strings.Contains(ne.Message, "not supported when using Codex") {
		t.Fatalf("err = %v, want 400 with the upstream words", err)
	}
}

// An app-server that fails before the first byte is retried once on exec.
func TestAppServerFailureBeforeFirstByteFallsBackToExec(t *testing.T) {
	r, home := fakeRunner(t, "protocol_broken")
	res, err := r.Run(context.Background(), simpleRequest(), nil)
	if err != nil || res.Text != "Bonjour !" {
		t.Fatalf("res=%+v err=%v, want the exec answer", res, err)
	}
	readExec(t, home)
}

// After the first byte the client holds part of an answer: no retry.
func TestAppServerFailureAfterFirstByteDoesNotFallBack(t *testing.T) {
	r, home := fakeRunner(t, "exit_midturn")
	if _, err := r.Run(context.Background(), simpleRequest(), func(Delta) error { return nil }); err == nil {
		t.Fatal("want the mid-turn failure")
	}
	if _, err := os.Stat(filepath.Join(home, "exec.json")); err == nil {
		t.Fatal("exec ran after the client already received bytes")
	}
}

func TestExecModeAnswersCatalogAndUsageWith404(t *testing.T) {
	r, _ := fakeRunner(t, "ok")
	r.Channel = ChannelExec
	_, err := r.Models(context.Background())
	var ne *NativeError
	if !errors.As(err, &ne) || ne.Status != 404 {
		t.Fatalf("models err = %v, want 404", err)
	}
	if _, err := r.RateLimits(context.Background()); !errors.As(err, &ne) || ne.Status != 404 {
		t.Fatalf("rate limits err = %v, want 404", err)
	}
}

func TestWriteDataURL(t *testing.T) {
	dir := t.TempDir()
	path, err := writeDataURL(dir, 1, "data:image/jpeg;base64,/9j/4AAQ")
	if err != nil || !strings.HasSuffix(path, "image-1.jpg") {
		t.Fatalf("path=%q err=%v", path, err)
	}
	for _, bad := range []string{"https://example.com/a.png", "data:image/png,raw", "data:image/png;base64,!!!"} {
		if _, err := writeDataURL(dir, 2, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
