package router

// The block cursor must retain WHY a block started. It existed only as a log
// line, so in the dashboard an early advance (eligibility flap) looked exactly
// like a scheduled rotation — the operator could see the position but not the
// cause.

import (
	"context"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestBlockCursorCarriesAdvanceReason(t *testing.T) {
	cat := withCombo(newFake(), blockCombo(2))
	r := New(cat)
	ctx := context.Background()

	if _, err := r.ResolveCandidates(ctx, "daily", provider.SurfaceChat, "test"); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, c := range r.BlockCursors() {
		if c.Combo != "daily" {
			continue
		}
		found = true
		switch c.Reason {
		case "cold start", "block complete", "block hop left candidates":
		default:
			t.Errorf("cursor reason = %q, want one of the three advance reasons", c.Reason)
		}
		if c.BlockSize != 2 {
			t.Errorf("cursor block_size = %d, want 2", c.BlockSize)
		}
		if c.Hop == "" {
			t.Error("a live cursor must name the hop owning the block")
		}
	}
	if !found {
		t.Fatal("no cursor for the block combo after a dispatch")
	}
}
