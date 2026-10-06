package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// handleStream is GET /admin/stream — the live dashboard feed (SSE).
//
// Why SSE and not WebSocket: the feed is one-directional (server -> dashboard),
// needs no framing handshake, survives plain reverse proxies, and — decisive —
// the browser's fetch() can read an SSE body while carrying the Authorization
// header. EventSource cannot set headers, so it would force the admin token
// into the query string, where it leaks into access logs.
//
// Wire format: standard SSE frames. Two event types:
//
//	event: snapshot   data: {"calls":[...]}     (on connect, newest first)
//	event: calls      data: {"calls":[...]}     (each committed ledger batch)
//	: ping                                         (heartbeat, comment frame)
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "server_error",
			"streaming unsupported by this connection")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // nginx: never buffer this response
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Subscribe BEFORE the snapshot so nothing committed between the snapshot
	// query and the live loop is lost; duplicates are harmless (the dashboard
	// keys rows by ts+model+tokens).
	batches, unsub := s.db.SubscribeLedger(64)
	defer unsub()

	// 1) snapshot: the last 20 calls, same shape the dashboard already renders.
	if recent, err := s.recentCalls(r.Context(), 20); err == nil {
		if err := writeSSE(w, flusher, "snapshot", map[string]any{
			"calls":   recent,
			"ts":      time.Now().UTC().Format(time.RFC3339Nano),
			"dropped": store.DroppedCalls(),
		}); err != nil {
			return
		}
	}

	// 2) live batches + heartbeat.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case batch, open := <-batches:
			if !open {
				return
			}
			events := make([]store.CallEvent, 0, len(batch))
			for _, c := range batch {
				events = append(events, store.EventFromCall(c))
			}
			if err := writeSSE(w, flusher, "calls", map[string]any{
				"calls": events,
				"ts":    time.Now().UTC().Format(time.RFC3339Nano),
			}); err != nil {
				return
			}
		}
	}
}

// writeSSE emits one `event: <name>` frame and flushes it immediately — a
// buffered SSE frame is an undelivered SSE frame.
func writeSSE(w http.ResponseWriter, f http.Flusher, name string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, payload); err != nil {
		return err
	}
	f.Flush()
	return nil
}
