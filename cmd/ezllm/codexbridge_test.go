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
	if _, err := startCodexBridge(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, ""); err != nil {
		t.Fatalf("off must be a no-op: %v", err)
	}
}

// On without a usable CLI is a boot failure naming the variable, not a
// healthy-looking process whose every request 503s.
func TestCodexBridgeOnWithoutTheCLIFailsBoot(t *testing.T) {
	t.Setenv(envCodexBridge, "on")
	t.Setenv(envCodexBin, filepath.Join(t.TempDir(), "no-codex"))
	_, err := startCodexBridge(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, "")
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
	first, err := bridgeTokenFromFile(path, "", "codex")
	if err != nil || len(first) != 48 {
		t.Fatalf("token = %q err = %v", first, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, want 0600", fi.Mode().Perm())
	}
	if again, _ := bridgeTokenFromFile(path, "", "codex"); again != first {
		t.Fatal("a second boot generated a new token: the account key would break on restart")
	}
}

// The tools policy (decision §4.7). The helper is a stub that fails its
// self-test, exactly as on a host whose kernel refuses Landlock.
func TestDecideCodexTools(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "no-landlock-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho refused >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := decideCodexTools(helper, bin); got.status != "disabled (code-mode host missing)" || got.off == "" {
		t.Fatalf("no code-mode host: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex-code-mode-host"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := decideCodexTools(helper, bin)
	if got.status != "disabled (no Landlock)" || !strings.HasPrefix(got.off, "Landlock is unavailable (") ||
		!strings.HasPrefix(got.landlock, "unavailable: ") || got.sandbox != nil {
		t.Fatalf("no Landlock: %+v", got)
	}
}

// A binary install's data folder is not /data: the token goes next to the
// ledger, wherever that is.
func TestBridgeTokenDefaultsToTheDataFolder(t *testing.T) {
	dir := t.TempDir()
	tok, err := bridgeTokenFromFile("", dir, "claude")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "claude-bridge.token"))
	if err != nil || strings.TrimSpace(string(b)) != tok {
		t.Fatalf("token not in the data folder: %v", err)
	}
}

// An empty file is replaced by a whole 0600 token; an unreadable one stops the
// start instead of being overwritten, which would break the stored account key.
func TestBridgeTokenReplacesEmptyRefusesUnreadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.token")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if tok, err := bridgeTokenFromFile(path, "", "codex"); err != nil || len(tok) != 48 {
		t.Fatalf("empty file: tok=%q err=%v", tok, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	locked := filepath.Join(dir, "locked.token")
	if err := os.WriteFile(locked, []byte("keep-me\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := bridgeTokenFromFile(locked, "", "codex"); err == nil {
		t.Fatal("an unreadable token file was replaced")
	}
	os.Chmod(locked, 0o600)
	if b, _ := os.ReadFile(locked); string(b) != "keep-me\n" {
		t.Fatalf("the unreadable token was overwritten: %q", b)
	}
}
