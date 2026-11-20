package server

import (
	"net/http"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// handleTurns serves GET /admin/turns: requests per agent-loop turn, per
// account and model (store.TurnLengths). Window [from, to), default 7 days.
func (s *Server) handleTurns(w http.ResponseWriter, r *http.Request) {
	from, to, ok := parseWindow(w, r, 7*24*time.Hour)
	if !ok {
		return
	}
	rows, err := s.db.TurnLengths(r.Context(), from, to)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339),
		"long_turn": store.LongTurn, "rows": rows,
	})
}

// turnChainSize bounds the response-id map: enough for every live stateful
// loop on one gateway, small enough to never matter for memory (~0.5 MiB).
const turnChainSize = 4096

// turnChain maps a Responses response id to the turn that produced it, so a
// stateful continuation (previous_response_id plus tool outputs, no human
// message) keeps its loop's turn_id. Keys are scoped by client: one client
// cannot join another's turn. Memory only: after a restart a continuation
// starts unknown (""), as before. Oldest ids are evicted first.
type turnChain struct {
	mu    sync.Mutex
	turns map[string]string
	order []string // ring of keys, oldest at next
	next  int
}

func (c *turnChain) remember(client, responseID, turn string) {
	if responseID == "" || turn == "" {
		return
	}
	k := client + "\x00" + responseID
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.turns == nil {
		c.turns = make(map[string]string, turnChainSize)
		c.order = make([]string, turnChainSize)
	}
	if _, ok := c.turns[k]; ok {
		c.turns[k] = turn
		return
	}
	if old := c.order[c.next]; old != "" {
		delete(c.turns, old)
	}
	c.order[c.next] = k
	c.next = (c.next + 1) % turnChainSize
	c.turns[k] = turn
}

func (c *turnChain) lookup(client, responseID string) string {
	if responseID == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turns[client+"\x00"+responseID]
}

// previousResponseID reads a Responses body's previous_response_id.
func previousResponseID(doc *jsonbody.BodyDoc) string {
	if doc == nil || doc.Err() != nil {
		return ""
	}
	root := doc.Map()
	if root == nil {
		return ""
	}
	id, _ := root["previous_response_id"].(string)
	return id
}
