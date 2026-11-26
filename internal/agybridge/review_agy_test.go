package agybridge

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// When the deltas stop short of the answer, the rest of the envelope's
// response is still streamed.
func TestRunStreamsTheTailAfterPartialDeltas(t *testing.T) {
	stream := `{"event":"step_update","conversation_id":"c1","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"po"}
{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"pong!","num_turns":1,"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	bin, _ := fakeAgy(t, stream, 0)
	r := &Runner{Bin: bin, Home: "/agy-sub", Timeout: 10 * time.Second}
	var got strings.Builder
	if _, err := r.Run(context.Background(), Request{Model: "m", Prompt: "p"}, func(s string) error { got.WriteString(s); return nil }); err != nil {
		t.Fatal(err)
	}
	if got.String() != "pong!" {
		t.Fatalf("streamed %q, want the whole answer pong!", got.String())
	}
}

// An output line over the reader's limit is reported as unreadable output.
func TestRunReportsUnreadableOutput(t *testing.T) {
	bin, dir := fakeAgy(t, "", 0)
	if err := os.WriteFile(dir+"/out", []byte(strings.Repeat("x", 9<<20)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Bin: bin, Home: "/agy-sub", Timeout: 20 * time.Second}
	_, err := r.Run(context.Background(), Request{Model: "m", Prompt: "p"}, nil)
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("err = %v, want unreadable output", err)
	}
}

// Back-to-back plan-usage reads start one CLI, not one each.
func TestPlanUsageReusesAFreshReading(t *testing.T) {
	bin, dir := fakeAgy(t, measuredUsage, 0)
	r := &Runner{Bin: bin, Home: dir}
	if _, err := r.PlanUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir + "/argv"); err != nil {
		t.Fatal(err)
	}
	got, err := r.PlanUsage(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("second read = %v, %v", got, err)
	}
	if _, err := os.Stat(dir + "/argv"); err == nil {
		t.Fatal("second read started the CLI again")
	}
}
