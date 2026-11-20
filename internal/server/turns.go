package server

import (
	"net/http"
	"time"

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
