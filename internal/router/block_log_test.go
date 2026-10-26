package router

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// logCapture swaps the process logger for a collecting handler and restores
// it on cleanup. Router cursor tests are the only users; they never run in
// parallel.
func logCapture(t *testing.T) *[]string {
	t.Helper()
	var lines []string
	h := slog.NewJSONHandler(&lineWriter{&lines}, &slog.HandlerOptions{Level: slog.LevelInfo})
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &lines
}

type lineWriter struct{ lines *[]string }

func (w *lineWriter) Write(p []byte) (int, error) {
	*w.lines = append(*w.lines, string(bytes.TrimSpace(p)))
	return len(p), nil
}

func blockCombo(size int) *store.Combo {
	return &store.Combo{
		ID: 12, Name: "daily", Strategy: "block_round_robin", Enabled: true, BlockSize: size,
		Hops: []store.ComboHop{
			{AccountID: 2, ModelID: "gpt-6-luna", Weight: 1},
			{AccountID: 2, ModelID: "gpt-6.1-sol", Weight: 1},
		},
	}
}

// The cursor is observable: exactly one advance line per block, never per
// request, with the reason a human needs.
func TestBlockAdvanceLoggedOncePerBlock(t *testing.T) {
	lines := logCapture(t)
	cat := withCombo(newFake(), blockCombo(3))
	r := New(cat)
	ctx := context.Background()

	// Cold start + first block of 3: one line (cold start).
	for i := 0; i < 3; i++ {
		if _, err := r.ResolveCandidates(ctx, "daily", provider.SurfaceChat, "test"); err != nil {
			t.Fatal(err)
		}
	}
	// Second block of 3: one more line (block complete).
	for i := 0; i < 3; i++ {
		if _, err := r.ResolveCandidates(ctx, "daily", provider.SurfaceChat, "test"); err != nil {
			t.Fatal(err)
		}
	}

	advances := 0
	for _, l := range *lines {
		if !strings.Contains(l, `"msg":"block advance"`) {
			continue
		}
		advances++
		if !strings.Contains(l, `"combo":"daily"`) || !strings.Contains(l, `"block_size":3`) {
			t.Errorf("advance line missing combo/size: %s", l)
		}
	}
	if advances != 2 {
		t.Fatalf("advance lines = %d, want 2 (cold start + one rotation):\n%s", advances, strings.Join(*lines, "\n"))
	}
	if !strings.Contains((*lines)[0], `"reason":"cold start"`) {
		t.Errorf("first advance reason = %s, want cold start", (*lines)[0])
	}
	if !strings.Contains((*lines)[1], `"reason":"block complete"`) {
		t.Errorf("second advance reason = %s, want block complete", (*lines)[1])
	}
	if !strings.Contains((*lines)[1], `"hop":"super-ssn/gpt-6.1-sol"`) {
		// The rotation must walk FORWARD to the next position, not restart.
		t.Errorf("second advance hop = %s, want super-ssn/gpt-6.1-sol", (*lines)[1])
	}

	// The same state the log line reports must be readable by the dashboard
	// through the resolver's cursor snapshot (the recent-calls table joins
	// rows to their combo's cursor by combo name).
	cursors := r.BlockCursors()
	found := false
	for _, c := range cursors {
		if c.Combo == "daily" {
			found = true
			if c.BlockSize != 3 || (c.Hop != "super-ssn/gpt-6-luna" && c.Hop != "super-ssn/gpt-6.1-sol") {
				t.Errorf("cursor = %+v, want block_size 3 on one of the hops", c)
			}
		}
	}
	if !found {
		t.Errorf("no cursor for combo daily in %v", cursors)
	}
}
