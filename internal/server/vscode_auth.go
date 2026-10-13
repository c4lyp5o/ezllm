package server

import (
	"context"
	"net/http"
	"strings"
)

type vscodeTokenKey struct{}

// vscodeAuthed extracts token from /api/v1/vscode/{token}/... path
// and validates it like bearer token auth.
func (s *Server) vscodeAuthed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Extract token from path: /api/v1/vscode/{token}/...
		path := r.URL.Path
		prefix := "/api/v1/vscode/"
		if !strings.HasPrefix(path, prefix) {
			writeErr(w, http.StatusUnauthorized, "invalid_request_error", "invalid vscode path")
			return
		}
		rest := strings.TrimPrefix(path, prefix)
		token := strings.Split(rest, "/")[0]
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "invalid_request_error", "missing vscode token")
			return
		}

		name, roles, err := s.auth.ClientByToken(r.Context(), token)
		if err != nil || name == "" {
			writeErr(w, http.StatusUnauthorized, "invalid_request_error", "invalid vscode token")
			return
		}

		ctx := context.WithValue(r.Context(), rolesKey{}, roles)
		ctx = context.WithValue(ctx, clientKey{}, name)
		if info := reqInfoOf(r); info != nil {
			info.client = name
		}
		next(w, r.WithContext(ctx))
	}
}
