package codexbridge

import (
	"context"
	"encoding/json"
	"time"
)

// Continuation (operator decision §4.6): a turn that stops on client tool calls
// keeps its child alive, the model's code-mode script blocked on the pending
// calls. The follow-up request — the client's tool results, matched by
// tool_call_id — answers those calls and the same script resumes with its
// state intact. A parked turn holds no concurrency slot; it is bounded by
// ToolHold and MaxParked, and a follow-up that finds nothing parked falls back
// to the stateless transcript replay.

type parkedTurn struct {
	c     *child
	st    *turnState
	ids   []string
	at    time.Time
	timer *time.Timer
}

func (r *Runner) toolHold() time.Duration {
	if r.ToolHold > 0 {
		return r.ToolHold
	}
	return 10 * time.Minute
}

func (r *Runner) maxParked() int {
	if r.MaxParked > 0 {
		return r.MaxParked
	}
	return 8
}

// park registers a stopped turn under each of its pending call ids.
func (r *Runner) park(c *child, st *turnState, calls []ToolCall) {
	pt := &parkedTurn{c: c, st: st, at: time.Now()}
	for _, call := range calls {
		pt.ids = append(pt.ids, call.ID)
	}
	r.pmu.Lock()
	if r.parked == nil {
		r.parked = map[string]*parkedTurn{}
	}
	var evict []*parkedTurn
	for len(r.uniqueParked()) >= r.maxParked() {
		oldest := r.oldestParked()
		r.unregister(oldest)
		evict = append(evict, oldest)
	}
	for _, id := range pt.ids {
		r.parked[id] = pt
	}
	pt.timer = time.AfterFunc(r.toolHold(), func() {
		r.pmu.Lock()
		live := r.parked[pt.ids[0]] == pt
		if live {
			r.unregister(pt)
		}
		r.pmu.Unlock()
		if live {
			r.logf("codex-bridge: parked tool turn expired without results", "calls", len(pt.ids))
			pt.c.close()
		}
	})
	r.pmu.Unlock()
	for _, e := range evict {
		r.logf("codex-bridge: parked tool turn evicted (MaxParked)", "calls", len(e.ids))
		e.c.close()
	}
}

// takeParked removes and returns the parked turn these results answer.
func (r *Runner) takeParked(results map[string]string) *parkedTurn {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	for id := range results {
		if pt, ok := r.parked[id]; ok {
			r.unregister(pt)
			pt.timer.Stop()
			return pt
		}
	}
	return nil
}

// unregister drops every id of pt; pmu must be held.
func (r *Runner) unregister(pt *parkedTurn) {
	for _, id := range pt.ids {
		if r.parked[id] == pt {
			delete(r.parked, id)
		}
	}
}

func (r *Runner) uniqueParked() map[*parkedTurn]bool {
	seen := map[*parkedTurn]bool{}
	for _, pt := range r.parked {
		seen[pt] = true
	}
	return seen
}

func (r *Runner) oldestParked() *parkedTurn {
	var oldest *parkedTurn
	for pt := range r.uniqueParked() {
		if oldest == nil || pt.at.Before(oldest.at) {
			oldest = pt
		}
	}
	return oldest
}

// ParkedTurns reports how many turns wait for tool results (for /status).
func (r *Runner) ParkedTurns() int {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	return len(r.uniqueParked())
}

// resume answers a parked turn's pending calls with the client's results and
// streams the rest of the turn — possibly to the next tool stop.
func (r *Runner) resume(ctx context.Context, pt *parkedTurn, req Request, sink func(Delta) error) (Result, error) {
	st := pt.st
	st.mu.Lock()
	st.sink = sink
	st.sinkErr = nil
	pending := st.replyTo
	st.replyTo = map[string]json.RawMessage{}
	st.calls = nil
	st.callsReady = false
	if st.batch != nil {
		st.batch.Stop()
		st.batch = nil
	}
	st.mu.Unlock()

	for callID, rpcID := range pending {
		text, ok := req.ToolResults[callID]
		if !ok {
			pt.c.conn.reply(rpcID, map[string]any{"success": false, "contentItems": []any{
				map[string]any{"type": "inputText", "text": "the client returned no result for this call"}}}, nil)
			continue
		}
		pt.c.conn.reply(rpcID, map[string]any{"success": true, "contentItems": []any{
			map[string]any{"type": "inputText", "text": text}}}, nil)
	}

	res, park, err := r.segment(ctx, pt.c, st)
	if err != nil {
		pt.c.close()
		return Result{}, err
	}
	if park {
		r.park(pt.c, st, res.ToolCalls)
		return res, nil
	}
	pt.c.close()
	return res, nil
}
