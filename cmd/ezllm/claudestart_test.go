package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func claudeStartEnv(t *testing.T, bin string) {
	t.Helper()
	t.Setenv(envBridge, "on")
	t.Setenv(envBridgeBin, bin)
	t.Setenv(envBridgeDir, t.TempDir())
	t.Setenv(envBridgeAddr, "127.0.0.1:0")
	t.Setenv(envBridgeToken, "")
	t.Setenv("USER", "operator")
	t.Setenv("LOGNAME", "operator")
}

// A CLI that cannot run, or a process the CLI cannot identify, stops the start:
// otherwise ezllm looks healthy while every Claude request fails.
func TestClaudeStartRefusesABrokenCLIAndMissingIdentity(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "claude")
	writeExec(t, broken, `exit 1`)
	claudeStartEnv(t, broken)
	if _, err := startClaudeBridge(context.Background(), slog.Default(), nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "does not run") {
		t.Fatalf("broken CLI: err = %v, want a refusal", err)
	}

	ok := filepath.Join(t.TempDir(), "claude")
	fakeClaude(t, ok, "2.1.306", "")
	claudeStartEnv(t, ok)
	t.Setenv("USER", "")
	if _, err := startClaudeBridge(context.Background(), slog.Default(), nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "USER and LOGNAME") {
		t.Fatalf("no USER: err = %v, want a refusal", err)
	}
}

// A missing login does not stop the start: the operator can sign in later.
func TestClaudeStartAllowsAMissingLogin(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	fakeClaude(t, bin, "2.1.306", "")
	claudeStartEnv(t, bin)
	stop, err := startClaudeBridge(context.Background(), slog.Default(), nil, t.TempDir())
	if err != nil {
		t.Fatalf("missing login stopped the start: %v", err)
	}
	stop()
}

// A CLI that hangs cannot hold the start once its context is cancelled.
func TestClaudeChecksStopWithTheStart(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	writeExec(t, bin, `exec sleep 60`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if v, _ := claudeCLICheck(ctx, bin); v != "unavailable" {
		t.Fatalf("version = %q, want unavailable", v)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("a hung --version held the start for %s", d)
	}
}
