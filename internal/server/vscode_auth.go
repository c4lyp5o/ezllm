package server

import (
	"net/http"
	"strings"
)

// vscodeAuthed extracts the token from /api/v1/vscode/{token}/... and runs
// the same gate as bearer auth (authenticate): identity, expiry and the
// per-key budget.
func (s *Server) vscodeAuthed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(r.PathValue("token"))
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "invalid_request_error", "missing vscode token")
			return
		}
		if r, ok := s.authenticate(w, r, token); ok {
			next(w, r)
		}
	}
}
