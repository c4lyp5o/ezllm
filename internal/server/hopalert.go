package server

import (
	"context"
	"fmt"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// A hop that commits a 200 and then dies mid-body looks healthy everywhere but
// the stream-outcome rollup (docs/hop-health-design.md). The dashboard shows
// it; this raises the bell for it, so a flaky hop is noticed without opening
// the dashboard. The thresholds are the ones the dashboard's attention item
// uses: more than 1% truncation across at least 20 judged streams.
const (
	hopAlertWindow    = 24 * time.Hour
	hopAlertMinJudged = 20
	hopAlertRate      = 0.01
	hopAlertEvery     = 15 * time.Minute
	hopAlertFirst     = 2 * time.Minute
)

// checkHopStreams raises one folded bell event per offending (account, model)
// hop. The dedup key is per hop, so a recurring cut is one line with a counter
// and the push channels repeat it no faster than their own pushRepeat floor.
func (s *Server) checkHopStreams(ctx context.Context, now time.Time) {
	hops, err := s.db.HopStreamHealth(ctx, now.Add(-hopAlertWindow), now)
	if err != nil {
		s.log.Warn("hop stream alert check failed", "err", err)
		return
	}
	for _, h := range hops {
		judged := h.Streams - h.ClientGone
		if judged < hopAlertMinJudged || h.Rate < hopAlertRate {
			continue
		}
		s.notify(store.NotificationInput{
			Kind:     store.NoteStreamTruncation,
			Severity: "warn",
			Title:    fmt.Sprintf("%s · %s is cutting answers", h.Namespace, h.Model),
			Body: fmt.Sprintf("%d of %d judged streams ended early (%.1f%%) in the last 24 hours; %d caller hang-ups are not counted.",
				h.Truncated, judged, h.Rate*100, h.ClientGone),
			Data:     h,
			DedupKey: "stream_truncation:" + h.Namespace + ":" + h.Model,
		})
	}
}

// startHopStreamAlerts runs the check on a slow ticker; the rollup is one
// indexed GROUP BY, so this costs nothing on the request path.
func (s *Server) startHopStreamAlerts(ctx context.Context) {
	if s.db == nil {
		return
	}
	s.goBackground(func(bg context.Context) {
		ctx, stop := either(ctx, bg)
		defer stop()
		timer := time.NewTimer(hopAlertFirst)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				s.checkHopStreams(ctx, time.Now().UTC())
				timer.Reset(hopAlertEvery)
			}
		}
	})
}
