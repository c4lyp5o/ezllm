package server

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

var loginRateLimiter = struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}{
	attempts: map[string][]time.Time{},
}

func loginClientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	return ip
}

func loginAllowed(ip string) bool {
	loginRateLimiter.mu.Lock()
	defer loginRateLimiter.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-15 * time.Minute)
	var recent []time.Time
	for _, t := range loginRateLimiter.attempts[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	loginRateLimiter.attempts[ip] = recent
	return len(recent) < 5
}

func recordLoginFailure(ip string) {
	loginRateLimiter.mu.Lock()
	defer loginRateLimiter.mu.Unlock()
	loginRateLimiter.attempts[ip] = append(loginRateLimiter.attempts[ip], time.Now())
}

func clearLoginAttempts(ip string) {
	loginRateLimiter.mu.Lock()
	defer loginRateLimiter.mu.Unlock()
	delete(loginRateLimiter.attempts, ip)
}

func (s *Server) handleDashboardLogin(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeErr(w, http.StatusServiceUnavailable, "server_error", "dashboard authentication unavailable")
		return
	}
	ip := loginClientIP(r)
	if !loginAllowed(ip) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts; try again later")
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
			if strings.TrimSpace(in.Password) != "" {
				recordLoginFailure(ip)
			}
			writeErrExtra(w, http.StatusUnauthorized, "invalid_request_error", "invalid dashboard password",
				map[string]any{"default_password": isDefault})
			return
		}
		s.writeStoreErr(w, err)
		return
	}
	clearLoginAttempts(ip)
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     "ezllm_session",
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   12 * 60 * 60,
	})
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
