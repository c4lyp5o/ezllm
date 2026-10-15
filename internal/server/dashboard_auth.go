package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func (s *Server) handleDashboardLogin(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeErr(w, http.StatusServiceUnavailable, "server_error", "dashboard authentication unavailable")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	isDefault := s.db.DashboardPasswordIsDefault(r.Context())
	token, err := s.db.DashboardLogin(r.Context(), in.Password)
	if err != nil {
		if errors.Is(err, store.ErrInvalidDashboardPassword) {
			// The flag rides even the failure so the page can decide whether
			// the first-run hint is still relevant BEFORE a successful login.
			writeErrExtra(w, http.StatusUnauthorized, "invalid_request_error", "invalid dashboard password",
				map[string]any{"default_password": isDefault})
			return
		}
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "default_password": isDefault})
}

func (s *Server) handleDashboardPassword(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeErr(w, http.StatusServiceUnavailable, "server_error", "dashboard authentication unavailable")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if err := s.db.ChangeDashboardPassword(r.Context(), in.CurrentPassword, in.NewPassword); err != nil {
		if errors.Is(err, store.ErrInvalidDashboardPassword) {
			writeErr(w, http.StatusUnauthorized, "invalid_request_error", "current password is incorrect")
			return
		}
		if strings.Contains(err.Error(), "at least 12 characters") {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		s.writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
