package server

// Regression guards for the "Waiting for authorization…" stall (2026-10-17).
// Two independent defects left the dashboard saying it was waiting while
// github.com/login/device already said Success, and neither was visible:
//   - slow_down was folded into plain pending, so GitHub kept withholding the
//     token and nothing in the response told the dashboard to slow down;
//   - 5xx/429/context errors were classified terminal, so ONE blip poisoned a
//     handle the human had already authorized.
// Both tests fail if either behaviour regresses.

import (
	"context"
	"errors"
	"testing"
)

// TestGitHubDeviceSlowDownIsSurfaced proves the flag, the pending status and an
// operator-readable detail all reach the dashboard. Without it the panel reads
// identically to an un-authorized flow.
func TestGitHubDeviceSlowDownIsSurfaced(t *testing.T) {
	f := &fakeGitHub{}
	f.slowDown.Store(true)
	h := githubHarness(t, f)

	handle := startGHFlow(t, h)
	st := ghStatus(t, h, handle)

	if st["status"] != "pending" {
		t.Fatalf("slow_down status = %v, want pending (the code is still live)", st["status"])
	}
	if st["slow_down"] != true {
		t.Fatalf("slow_down flag = %v, want true — the dashboard never learns to slow down", st["slow_down"])
	}
	detail, _ := st["detail"].(string)
	if detail == "" {
		t.Fatal("slow_down carries no detail — the operator cannot tell backoff from a hang")
	}
	// It must stay non-terminal: the flow is alive, GitHub is asking us to wait.
	if st["error"] != nil {
		t.Fatalf("slow_down treated as terminal: %v", st["error"])
	}
}

// TestDeviceErrClassification pins which poll failures kill a flow and which
// are retried. The bug was that an upstream 5xx or our own cancelled context
// was read as "GitHub said no" and permanently poisoned a granted handle.
func TestDeviceErrClassification(t *testing.T) {
	retry := []error{
		context.Canceled,
		context.DeadlineExceeded,
		errors.New("githubcopilot: https://github.com/login/oauth/access_token: status 503: Service Unavailable"),
		errors.New("githubcopilot: https://github.com/login/oauth/access_token: status 429: Too Many Requests"),
		errors.New("Post \"https://github.com/login/oauth/access_token\": connection refused"),
		errors.New("githubcopilot: token poll: context deadline exceeded"),
	}
	for _, err := range retry {
		if isTerminalDeviceErr(err) {
			t.Errorf("transient error classified terminal: %v", err)
		}
	}

	terminal := []error{
		errors.New("githubcopilot: device flow access_denied"),
		errors.New("githubcopilot: device flow expired_token"),
		errors.New("githubcopilot: device flow incorrect_client_credentials"),
		errors.New("githubcopilot: https://github.com/login/oauth/access_token: status 400: Bad Request"),
	}
	for _, err := range terminal {
		if !isTerminalDeviceErr(err) {
			t.Errorf("genuine refusal classified transient (would loop forever): %v", err)
		}
	}
}
