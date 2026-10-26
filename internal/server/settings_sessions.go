package server

import "net/http"

func dashboardCredential(r *http.Request) string {
	if token := bearerToken(r); token != "" {
		return token
	}
	if c, err := r.Cookie("ezllm_session"); err == nil {
		return c.Value
	}
	return ""
}
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.ListDashboardSessions(r.Context(), dashboardCredential(r))
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, rows)
}
func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	if err := s.db.RevokeDashboardSessionID(r.Context(), r.PathValue("id")); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	w.WriteHeader(204)
}
func (s *Server) handleRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	if err := s.db.RevokeAllDashboardSessions(r.Context()); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	w.WriteHeader(204)
}
