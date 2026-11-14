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
	caller     string
	hasHistory bool
	parentHash [32]byte
	c          *child
	st         *turnState
	ids        []string
	at         time.Time
	timer      *time.Timer
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
func (r *Runner) park(c *child, st *turnState, res Result) {
	calls := res.ToolCalls
	pt := &parkedTurn{c: c, st: st, at: time.Now(), caller: st.caller, hasHistory: st.hasHistory, parentHash: st.historyHash}
	r.pmu.Lock()
	if r.shut {
		r.pmu.Unlock()
		c.dispose() // shutting down: nothing will ever resume it
		return
	}
	if r.parked == nil {
		r.parked = map[string]*parkedTurn{}
	}
	var evict []*parkedTurn
	for len(r.uniqueParked()) >= r.maxParked() {
		oldest := r.oldestParked()
		r.unregister(oldest)
		evict = append(evict, oldest)
	}
	// A CLI may recycle native IDs in another child. Never overwrite another
	// parked conversation; aliases are client-facing only, replies use RPC IDs.
	st.mu.Lock()
	for i := range calls {
		original := calls[i].ID
		if r.parked[original] != nil {
			alias := "call_" + randomHex(16)
			for r.parked[alias] != nil {
				alias = "call_" + randomHex(16)
			}
			rpcID := st.replyTo[original]
			delete(st.replyTo, original)
			st.replyTo[alias] = rpcID
			calls[i].ID = alias
		}
		pt.ids = append(pt.ids, calls[i].ID)
	}
	if st.hasHistory {
		pt.parentHash = assistantHash(st.historyHash, res)
	}
	st.mu.Unlock()
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
			pt.c.dispose()
		}
	})
	r.pmu.Unlock()
	for _, e := range evict {
		r.logf("codex-bridge: parked tool turn evicted (MaxParked)", "calls", len(e.ids))
		e.c.dispose()
	}
}

// takeParked removes and returns the parked turn these results answer.
func (r *Runner) takeParked(req Request) (*parkedTurn, error) {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	for id := range req.ToolResults {
		if pt, ok := r.parked[id]; ok {
			if pt.caller != req.CallerID {
				return nil, &NativeError{Status: 403, Type: "permission_error", Code: "tool_session_mismatch", Message: "codex-bridge: tool continuation does not belong to this caller"}
			}
			hash := req.toolParentHash
			if !req.hasHistory {
				hash = inputHash(req.Input)
			}
			if pt.hasHistory != req.hasHistory || pt.parentHash != hash {
				return nil, nil
			}
			r.unregister(pt)
			pt.timer.Stop()
			return pt, nil
		}
	}
	return nil, nil
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
	pt.c.bind(ctx) // the follow-up request now owns the child's lifetime
	st := pt.st
	st.mu.Lock()
	st.sink = sink
	if req.hasHistory {
		st.historyHash = req.historyHash
	}
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
		pt.c.dispose()
		return Result{}, err
	}
	if park {
		if !pt.c.release() {
			pt.c.dispose()
			return Result{}, r.childErr(ctx, pt.c, "turn", ctx.Err())
		}
		r.park(pt.c, st, res)
		return res, nil
	}
	if pt.c.lease != nil && ctx.Err() == nil && validTemplateUsage(pt.st.usage) {
		pt.c.lease.good.Store(true)
	}
	pt.c.dispose()
	return res, nil
}

// Shutdown reaps every parked child and stops their hold timers; a turn that
// stops on tool calls afterwards is closed instead of parked. Parked children
// outlive their HTTP request by design, so an HTTP drain alone never reached
// them. Active runs end through their own request contexts.
func (r *Runner) Shutdown() {
	r.shutdownTemplates()
	r.pmu.Lock()
	r.shut = true
	var reap []*parkedTurn
	for pt := range r.uniqueParked() {
		r.unregister(pt)
		pt.timer.Stop()
		reap = append(reap, pt)
	}
	r.pmu.Unlock()
	for _, pt := range reap {
		pt.c.dispose()
	}
	r.waitTemplateCleanup()
}
