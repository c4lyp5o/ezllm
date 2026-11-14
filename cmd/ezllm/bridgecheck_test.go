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

// fakeClaude answers --version and auth status like the real CLI, and parses
// `-p FLAG` like it: an unknown option names itself, a known one stops at the
// missing prompt. drop is a flag this release no longer accepts.
func fakeClaude(t *testing.T, bin, version, drop string) {
	writeExec(t, bin, `case "$1" in
--version) echo "`+version+` (Claude Code)";;
-p) if [ "$2" = "`+drop+`" ]; then echo "error: unknown option '$2'"; else echo "Error: Input must be provided"; fi; exit 1;;
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
	// "copilot" is a real target now; an unknown name must still be a usage error.
	for _, args := range [][]string{nil, {"--json"}, {"nope"}} {
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

func TestJudgeTurn(t *testing.T) {
	for _, c := range []struct {
		name, text, want, got string
		err                   error
		out                   int64
		pass                  bool
		says                  string
	}{
		{name: "plain ok", text: "Pong.", out: 3, pass: true},
		{name: "plain off-prompt", text: "hello", out: 3, says: "did not follow"},
		{name: "no usage", text: "pong", out: 0, says: "no output tokens"},
		{name: "native error", err: errors.New("rate limited"), says: "rate limited"},
		{name: "tool ok", want: "get_weather", got: "get_weather", out: 9, pass: true},
		{name: "tool stop reports no usage", want: "get_weather", got: "get_weather", out: 0, pass: true},
		{name: "tool missing", want: "get_weather", out: 9, says: "got none"},
		{name: "wrong tool", want: "get_weather", got: "Bash", out: 9, says: "got Bash"},
	} {
		p := judgeTurn("turn", c.err, c.text, c.out, c.want, c.got)
		if (p == "") != c.pass || !strings.Contains(p, c.says) {
			t.Errorf("%s: verdict %q, want pass=%v containing %q", c.name, p, c.pass, c.says)
		}
		if c.text != "" && c.text != "pong" && strings.Contains(p, c.text) {
			t.Errorf("%s: the model's text leaked into the verdict: %q", c.name, p)
		}
	}
}

// --live spends quota, so it runs only on a CLI that passed every static check;
// on one that did, a CLI that cannot answer a turn is refused.
func TestBridgeCheckLiveRunsOnlyAfterStaticPasses(t *testing.T) {
	t.Setenv("USER", "calypso")
	t.Setenv("LOGNAME", "calypso")
	bin := filepath.Join(t.TempDir(), "claude")
	login := t.TempDir()
	if err := os.WriteFile(filepath.Join(login, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeClaude(t, bin, "2.1.306", "--max-turns")
	v, err := checkJSON(t, "claude", "--bin", bin, "--home", login, "--live")
	if !errors.Is(err, errBridgeCheckFailed) || v.Live || len(v.Problems) != 1 {
		t.Fatalf("static failure: live=%v problems=%v, want the static refusal only", v.Live, v.Problems)
	}

	fakeClaude(t, bin, "2.1.306", "") // answers no turn: -p stops at the missing prompt
	v, err = checkJSON(t, "claude", "--bin", bin, "--home", login, "--live")
	if !errors.Is(err, errBridgeCheckFailed) || !v.Live || len(v.Problems) != 2 ||
		!strings.HasPrefix(v.Problems[0], "live plain turn") || !strings.HasPrefix(v.Problems[1], "live tool turn") {
		t.Fatalf("mute CLI: live=%v problems=%v, want both live turns refused", v.Live, v.Problems)
	}
}

// The bridge root is the folder holding `current`; a legacy, version-pinned
// mount has none, so nothing is watched there.
func TestBridgeUpdateFile(t *testing.T) {
	for bin, want := range map[string]string{
		"/opt/claude-bridge/current/claude":   "/opt/claude-bridge/update-status.json",
		"/opt/codex-bridge/current/bin/codex": "/opt/codex-bridge/update-status.json",
		"/opt/agy-bridge/current/agy":         "/opt/agy-bridge/update-status.json",
		"/opt/claude/claude":                  "",
		"/opt/codex/bin/codex":                "",
		"claude":                              "",
	} {
		if got := bridgeUpdateFile(bin); got != want {
			t.Errorf("bridgeUpdateFile(%q) = %q, want %q", bin, got, want)
		}
	}
}

// The copilot target must be routable and must fail closed: a binary that is not
// there is a problem, not a panic and not a silent pass. The check's own logic
// (protocol, login, live turns) is tested in internal/copilotbridge against a
// subprocess stub, so this pins the wiring only.
func TestBridgeCheckCopilotRoutesAndFailsClosed(t *testing.T) {
	v, err := checkJSON(t, "copilot", "--bin", filepath.Join(t.TempDir(), "nope"))
	if !errors.Is(err, errBridgeCheckFailed) {
		t.Fatalf("err = %v, want errBridgeCheckFailed", err)
	}
	if len(v.Problems) == 0 || !strings.Contains(v.Problems[0], "does not resolve") {
		t.Fatalf("problems = %v, want a resolution failure", v.Problems)
	}
}

func TestBridgeCheckReportsConfiguredPolicy(t *testing.T) {
	t.Setenv("EZLLM_CODEX_SANDBOX", "writable")
	t.Setenv("EZLLM_AGY_EXPERIMENTAL_TOOLS", "on")
	t.Setenv("EZLLM_COPILOT_BRIDGE_WARM_POOL", "on")
	for _, tc := range []struct{ name, key, want string }{
		{"codex", "sandbox", "workspace-write"}, {"agy", "caller_tools", "structured-output"}, {"copilot", "warm_pool", "on"},
	} {
		v, err := checkJSON(t, tc.name, "--bin", filepath.Join(t.TempDir(), "missing"))
		if !errors.Is(err, errBridgeCheckFailed) || v.Policy[tc.key] != tc.want {
			t.Fatalf("%s: %+v err=%v", tc.name, v, err)
		}
	}
}

func TestBridgeOperationalFailuresAreRetryableButCompatibilityFailuresAreNot(t *testing.T) {
	for _, tc := range []struct {
		problems []string
		want     bool
	}{
		{nil, false}, {[]string{"live plain: You've hit your session limit"}, true},
		{[]string{"live tool: rate limited", "live plain: quota exhausted"}, true},
		{[]string{"live plain: context deadline exceeded"}, true},
		{[]string{"login logged-out"}, true},
		{[]string{"flag missing --json-schema"}, false},
		{[]string{"live tool turn: no get_weather tool call came back"}, false},
		{[]string{"live plain: quota exhausted", "code-mode host missing"}, false},
	} {
		if got := retryableBridgeProblems(tc.problems); got != tc.want {
			t.Fatalf("%v: got %v want %v", tc.problems, got, tc.want)
		}
	}
}

func TestAgyGateRequiresSchemaFlagWhenExperimentalToolsEnabled(t *testing.T) {
	t.Setenv("EZLLM_AGY_EXPERIMENTAL_TOOLS", "on")
	home := t.TempDir()
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "antigravity-oauth-token"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "agy")
	for _, schema := range []bool{false, true} {
		help := "--print --output-format --input-format --model --effort --disable-slash-commands "
		if schema {
			help += "--json-schema "
		}
		writeExec(t, bin, `case "$1" in
--version) echo '1.4.12';;
--help) echo '`+help+`';;
models) printf 'gemini-3.8-flash-low\tGemini\n';;
esac
`)
		v, err := checkJSON(t, "agy", "--bin", bin, "--home", home)
		if schema {
			if err != nil || !v.OK {
				t.Fatalf("capable CLI refused: %+v %v", v, err)
			}
		} else if !errors.Is(err, errBridgeCheckFailed) || v.OK {
			t.Fatalf("schema-less CLI qualified: %+v %v", v, err)
		}
	}
}

func TestAgyCacheGateRequiresConversationFlag(t *testing.T) {
	t.Setenv(envAgyTools, "on")
	t.Setenv(envAgySessionCache, "on")
	home := t.TempDir()
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "antigravity-oauth-token"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "agy")
	for _, resume := range []bool{false, true} {
		help := "--print --output-format --input-format --model --effort --disable-slash-commands --json-schema "
		if resume {
			help += "--conversation "
		}
		writeExec(t, bin, `case "$1" in
--version) echo '1.4.12';;
--help) echo '`+help+`';;
models) printf 'gemini-3.8-flash-low\tGemini\n';;
esac
`)
		v, err := checkJSON(t, "agy", "--bin", bin, "--home", home)
		if resume {
			if err != nil || !v.OK || v.Policy["session_cache"] != "conversation-resume" {
				t.Fatalf("cache-capable CLI refused: %+v %v", v, err)
			}
		} else if !errors.Is(err, errBridgeCheckFailed) || v.OK {
			t.Fatalf("resume-less CLI qualified: %+v %v", v, err)
		}
	}
}
