package rules

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type failingRefreshSource struct {
	called chan struct{}
	once   sync.Once
}

func (s *failingRefreshSource) AllRules(context.Context) ([]Rule, error) {
	s.once.Do(func() { close(s.called) })
	return nil, errors.New("refresh failed")
}

type lockedLogBuffer struct {
	sync.Mutex
	bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}
func (b *lockedLogBuffer) text() string { b.Lock(); defer b.Unlock(); return b.Buffer.String() }

func TestRefreshFailureLogsAtWarning(t *testing.T) {
	for _, tc := range []struct {
		level slog.Level
		want  bool
	}{{slog.LevelWarn, true}, {slog.LevelError, false}} {
		t.Run(tc.level.String(), func(t *testing.T) {
			old := slog.Default()
			var out lockedLogBuffer
			slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: tc.level})))
			defer slog.SetDefault(old)
			src := &failingRefreshSource{called: make(chan struct{})}
			e := newEngine(src, &fakeUsage{}, nil, time.Millisecond)
			e.start()
			select {
			case <-src.called:
			case <-time.After(time.Second):
				t.Fatal("refresh did not run")
			}
			e.Close()
			if got := strings.Contains(out.text(), "rules refresh failed"); got != tc.want {
				t.Fatalf("log = %q", out.text())
			}
		})
	}
}
