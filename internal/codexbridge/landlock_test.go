package codexbridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func requireLandlock(t *testing.T) {
	t.Helper()
	if _, err := LandlockABI(); err != nil {
		t.Skip("no Landlock on this host: " + err.Error())
	}
}

func TestProbeLandlockProvesEnforcement(t *testing.T) {
	requireLandlock(t)
	abi, err := ProbeLandlock(os.Args[0])
	if err != nil || abi < 1 {
		t.Fatalf("abi=%d err=%v", abi, err)
	}
}

// The guarantee itself: a child started through the sandbox cannot write
// outside its work dir and CODEX_HOME, and cannot read files elsewhere — the
// fake app-server attempts both from inside the domain.
func TestSandboxedChildCannotEscape(t *testing.T) {
	requireLandlock(t)
	r, home := fakeRunner(t, "write_probe")
	r.Sandbox = &Sandbox{Helper: os.Args[0]}
	outside := t.TempDir()
	secret := filepath.Join(outside, "master.key")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(outside, "escaped.txt")
	if err := os.WriteFile(filepath.Join(home, "probe_target"), []byte(escape+"\n"+secret), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), simpleRequest(), nil)
	if err != nil || res.Text != "pong" {
		t.Fatalf("a sandboxed child must still work: text=%q err=%v", res.Text, err)
	}
	b, err := os.ReadFile(filepath.Join(home, "probe_result.json"))
	if err != nil {
		t.Fatalf("the probe did not run inside the child: %v", err)
	}
	var got struct{ Write, Read string }
	_ = json.Unmarshal(b, &got)
	if !strings.Contains(got.Write, "permission denied") || !strings.Contains(got.Read, "permission denied") {
		t.Fatalf("escape attempts: write=%q read=%q, want both denied", got.Write, got.Read)
	}
	if _, err := os.Stat(escape); !os.IsNotExist(err) {
		t.Fatal("a file appeared outside the domain")
	}
	_, env, _, _ := readStarted(t, home)
	if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "TMPDIR=") && strings.Contains(kv, "codex-bridge-") }) {
		t.Fatalf("TMPDIR must point inside the work dir under Landlock: %v", env)
	}
}

func TestSandboxWrapArgv(t *testing.T) {
	s := &Sandbox{Helper: "/app/ezllm"}
	bin, argv := s.wrap("/opt/codex/codex", []string{"app-server", "--stdio"}, "/tmp/codex-bridge-1", "/codex-sub")
	joined := strings.Join(argv, " ")
	if bin != "/app/ezllm" || argv[0] != LandlockHelperArg ||
		!strings.Contains(joined, "--rw /tmp/codex-bridge-1 --rw /codex-sub") ||
		!strings.Contains(joined, "--ro /opt/codex ") || !strings.HasSuffix(joined, "-- /opt/codex/codex app-server --stdio") {
		t.Fatalf("argv = %v", argv)
	}
	for _, forbidden := range []string{"/data", "/app", "/claude-sub", "--ro /opt "} {
		if strings.Contains(joined+" ", forbidden+" ") {
			t.Errorf("the domain grants %s", forbidden)
		}
	}
}
