package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The updater's layout: root/<ver>/bin/codex, root/current -> <ver>, and the
// record in root. The walk is lexical, so the symlink leads to the root.
func TestReleaseStateReadsTheUpdaterRecord(t *testing.T) {
	root := t.TempDir()
	writeExec(t, filepath.Join(root, "0.170.0", "bin", "codex"), "")
	if err := os.Symlink("0.170.0", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "current", "bin", "codex")

	if got := releaseState(bin, "0.163.0", "0.163.0"); got != "qualified" {
		t.Fatalf("measured version = %q, want qualified", got)
	}
	if got := releaseState(bin, "0.170.0", "0.163.0"); !strings.HasPrefix(got, "unqualified (measured on 0.163.0") {
		t.Fatalf("no record = %q, want unqualified", got)
	}
	rec := `{"versions":{"0.170.0":{"qualified_at":"2026-10-31T03:12:40Z","check":"live","proof":"7/7"}}}`
	if err := os.WriteFile(filepath.Join(root, qualifiedFile), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := releaseState(bin, "0.170.0", "0.163.0"); got != "qualified (live proof recorded by the updater)" {
		t.Fatalf("recorded version = %q, want qualified by the updater", got)
	}
	if got := releaseState(bin, "0.171.0", "0.163.0"); !strings.HasPrefix(got, "unqualified (measured") {
		t.Fatalf("unlisted version = %q, want unqualified", got)
	}
	if err := os.WriteFile(filepath.Join(root, qualifiedFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := releaseState(bin, "0.170.0", "0.163.0"); !strings.Contains(got, "unreadable") {
		t.Fatalf("malformed record = %q, want unreadable", got)
	}
}

// The walk stops at the bridge root: a record further up is not this bridge's.
func TestRecordQualifiesIsBounded(t *testing.T) {
	top := t.TempDir()
	if err := os.WriteFile(filepath.Join(top, qualifiedFile), []byte(`{"versions":{"1.0":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(top, "a", "b", "c", "cli")
	writeExec(t, bin, "")
	if ok, err := recordQualifies(bin, "1.0"); ok || err != nil {
		t.Fatalf("record four levels up = %v, %v; want not found", ok, err)
	}
}

// fakeClaude answers --version, --help (all required flags unless one is
// dropped) and auth status, like the real CLI's text output.
func fakeClaude(t *testing.T, bin, version, drop string) {
	var help strings.Builder
	for _, f := range claudeRequiredFlags {
		if f != drop {
			help.WriteString("  " + f + " <value>\n")
		}
	}
	writeExec(t, bin, `case "$1" in
--version) echo "`+version+` (Claude Code)";;
--help) cat <<'EOF'
`+help.String()+`EOF
;;
auth) echo "Logged in: true"; echo "Subscription: max";;
esac
`)
}

func checkJSON(t *testing.T, args ...string) (bridgeVerdict, error) {
	t.Helper()
	var out bytes.Buffer
	err := runBridgeCheck(append(args, "--json"), &out)
	var v bridgeVerdict
	if jerr := json.Unmarshal(out.Bytes(), &v); jerr != nil {
		t.Fatalf("verdict %q: %v", out.String(), jerr)
	}
	return v, err
}

// Boot reports flag drift and serves; the gate refuses to promote on it.
func TestBridgeCheckClaudeRefusesFlagDrift(t *testing.T) {
	t.Setenv("USER", "calypso")
	t.Setenv("LOGNAME", "calypso")
	root := t.TempDir()
	bin := filepath.Join(root, "current", "claude")
	login := t.TempDir()
	if err := os.WriteFile(filepath.Join(login, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeClaude(t, bin, "2.1.400", "--max-turns")
	v, err := checkJSON(t, "claude", "--bin", bin, "--home", login)
	if !errors.Is(err, errBridgeCheckFailed) || v.OK {
		t.Fatalf("drift: err = %v ok = %v, want a refusal", err, v.OK)
	}
	if len(v.Problems) != 1 || !strings.Contains(v.Problems[0], "--max-turns") {
		t.Fatalf("problems = %v, want the dropped flag named", v.Problems)
	}
	if v.Version != "2.1.400" || !strings.HasPrefix(v.Release, "unqualified") {
		t.Fatalf("version %q release %q, want 2.1.400 unqualified", v.Version, v.Release)
	}

	fakeClaude(t, bin, "2.1.400", "")
	if v, err := checkJSON(t, "claude", "--bin", bin, "--home", login); err != nil || !v.OK || len(v.Problems) != 0 {
		t.Fatalf("clean CLI: err = %v verdict = %+v, want ok", err, v)
	}
}

func TestBridgeCheckClaudeRefusesAMissingLogin(t *testing.T) {
	t.Setenv("USER", "calypso")
	t.Setenv("LOGNAME", "calypso")
	bin := filepath.Join(t.TempDir(), "claude")
	fakeClaude(t, bin, "2.1.306", "")
	v, err := checkJSON(t, "claude", "--bin", bin, "--home", t.TempDir())
	if !errors.Is(err, errBridgeCheckFailed) || len(v.Problems) != 1 || !strings.Contains(v.Problems[0], "logged-out") {
		t.Fatalf("err = %v problems = %v, want a logged-out refusal", err, v.Problems)
	}
	if v.Release != "qualified" {
		t.Fatalf("release = %q, want qualified for the measured version", v.Release)
	}
}

func TestBridgeCheckUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"--json"}, {"copilot"}} {
		if err := runBridgeCheck(args, &bytes.Buffer{}); err == nil || errors.Is(err, errBridgeCheckFailed) {
			t.Errorf("args %v: err = %v, want a usage error", args, err)
		}
	}
}

func TestClaudeVersion(t *testing.T) {
	for raw, want := range map[string]string{"2.1.306 (Claude Code)": "2.1.306", "2.1.306": "2.1.306", "": ""} {
		if got := claudeVersion(raw); got != want {
			t.Errorf("claudeVersion(%q) = %q, want %q", raw, got, want)
		}
	}
}
