package agybridge

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Live checks against the real CLI and the operator's Antigravity login. A turn
// spends plan quota, so they run only with EZLLM_AGY_LIVE=1, against
// EZLLM_AGY_BIN (default ~/.local/share/agy-bridge/<qualified>/agy) and
// EZLLM_AGY_HOME (default ~/.agy-sub).
func liveCLI(t *testing.T) (bin, home string) {
	t.Helper()
	if os.Getenv("EZLLM_AGY_LIVE") != "1" {
		t.Skip("set EZLLM_AGY_LIVE=1 to spend plan quota on live checks")
	}
	userHome, _ := os.UserHomeDir()
	bin = os.Getenv("EZLLM_AGY_BIN")
	if bin == "" {
		bin = userHome + "/.local/share/agy-bridge/" + QualifiedVersion + "/agy"
	}
	home = os.Getenv("EZLLM_AGY_HOME")
	if home == "" {
		home = userHome + "/.agy-sub"
	}
	return bin, home
}

func TestLivePreflight(t *testing.T) {
	bin, home := liveCLI(t)
	p, err := RunPreflight(context.Background(), bin, home)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Qualified || len(p.Models) == 0 {
		t.Fatalf("preflight %+v", p)
	}
	t.Logf("agy %s, %d models", p.Version, len(p.Models))
}

// TestLiveTurn passes on an eligible login, and on one Google has not yet
// cleared it must fail as 403 account_ineligible with the sign-in link withheld.
func TestLiveTurn(t *testing.T) {
	bin, home := liveCLI(t)
	model := os.Getenv("EZLLM_AGY_MODEL")
	if model == "" {
		model = "gemini-3.8-flash-low"
	}
	r := &Runner{Bin: bin, Home: home, Timeout: 120 * time.Second}
	var deltas int
	start := time.Now()
	res, err := r.Run(context.Background(), Request{Model: model, Prompt: "Reply with exactly: pong"},
		func(string) error { deltas++; return nil })
	if err != nil {
		ne := asNative(err)
		if ne.Code == "account_ineligible" && !strings.Contains(ne.Message, "https://") {
			t.Skipf("login not yet eligible (403 as designed, link withheld): %s", ne.Message)
		}
		t.Fatalf("%+v", ne)
	}
	if !strings.Contains(strings.ToLower(res.Text), "pong") || deltas == 0 || res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
		t.Fatalf("text=%q deltas=%d usage=%+v", res.Text, deltas, res.Usage)
	}
	t.Logf("wall %s, %d deltas, usage %+v, tool steps %d", time.Since(start).Round(time.Millisecond), deltas, res.Usage, res.ToolSteps)
}
