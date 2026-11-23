package bridgeutil

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func fullSlots(t *testing.T, log *slog.Logger) (*Slots, func()) {
	t.Helper()
	s := NewSlots(1, 1).Named("claude")
	s.log = log
	hold, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = s.Acquire(ctx) }()
	for s.Queued() == 0 {
		time.Sleep(time.Millisecond)
	}
	return s, func() { cancel(); hold() }
}

// A refusal logs one Warn naming the bridge; later refusals inside the window
// are counted, not logged.
func TestQueueFullWarnsOncePerWindow(t *testing.T) {
	var buf bytes.Buffer
	s, done := fullSlots(t, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer done()
	for range 3 {
		if _, err := s.Acquire(context.Background()); err != ErrQueueFull {
			t.Fatalf("err = %v, want ErrQueueFull", err)
		}
	}
	out := buf.String()
	if n := strings.Count(out, "bridge queue full"); n != 1 {
		t.Fatalf("warnings = %d, want 1 per window:\n%s", n, out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "bridge=claude") || !strings.Contains(out, "max_queue=1") {
		t.Fatalf("warning lacks level or fields: %s", out)
	}
	if s.quiet.Load() != 2 {
		t.Fatalf("suppressed = %d, want 2", s.quiet.Load())
	}
	// Next window: the line reports what was suppressed.
	buf.Reset()
	s.lastWarn.Store(1)
	_, _ = s.Acquire(context.Background())
	if !strings.Contains(buf.String(), "refused_since_last_warning=2") || !strings.Contains(buf.String(), "rejected_total=4") {
		t.Fatalf("second warning = %s", buf.String())
	}
}

// Above Warn (an operator running at error level) the line is filtered.
func TestQueueFullWarningFilteredAboveWarn(t *testing.T) {
	var buf bytes.Buffer
	s, done := fullSlots(t, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer done()
	_, _ = s.Acquire(context.Background())
	if buf.Len() != 0 {
		t.Fatalf("logged at error level: %s", buf.String())
	}
}
