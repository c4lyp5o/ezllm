package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexBridgeIsOffByDefault(t *testing.T) {
	t.Setenv(envCodexBridge, "")
	if err := startCodexBridge(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("off must be a no-op: %v", err)
	}
}

// On without a usable CLI is a boot failure naming the variable, not a
// healthy-looking process whose every request 503s.
func TestCodexBridgeOnWithoutTheCLIFailsBoot(t *testing.T) {
	t.Setenv(envCodexBridge, "on")
	t.Setenv(envCodexBin, filepath.Join(t.TempDir(), "no-codex"))
	err := startCodexBridge(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), envCodexBin) {
		t.Fatalf("err = %v, want a boot failure naming %s", err, envCodexBin)
	}
}

func TestCodexBridgeRefusesANonLoopbackBind(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8791": true, "[::1]:8791": true, "localhost:8791": true,
		"0.0.0.0:8791": false, "10.0.0.5:8791": false, ":8791": false, "nonsense": false,
	} {
		if err := assertCodexLoopback(addr); (err == nil) != ok {
			t.Errorf("assertCodexLoopback(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

func TestCodexBridgeTokenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-bridge.token")
	first, err := codexBridgeToken(path)
	if err != nil || len(first) != 48 {
		t.Fatalf("token = %q err = %v", first, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, want 0600", fi.Mode().Perm())
	}
	if again, _ := codexBridgeToken(path); again != first {
		t.Fatal("a second boot generated a new token: the account key would break on restart")
	}
}
