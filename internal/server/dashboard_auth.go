package server

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
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

// bcryptSlots bounds concurrent password hashing across login, setup and
// password change. The per-IP limiter cannot stop many source addresses
// (or every client behind one proxy address sharing a bucket) from running
// bcrypt in parallel; this caps the CPU an unauthenticated flood can take.
var bcryptSlots = make(chan struct{}, 4)

// bcryptWait is how long a request queues for a slot before it is refused.
// A var so tests can shorten it.
var bcryptWait = 2 * time.Second

// acquireBcrypt takes a hashing slot, or writes 429 + Retry-After and
// reports false when none frees up within bcryptWait.
func acquireBcrypt(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	timer := time.NewTimer(bcryptWait)
	defer timer.Stop()
	select {
	case bcryptSlots <- struct{}{}:
		return func() { <-bcryptSlots }, true
	case <-timer.C:
	case <-r.Context().Done():
	}
	w.Header().Set("Retry-After", "1")
	writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many sign-in attempts in progress; retry shortly")
	return nil, false
}

// setupCodeAlphabet drops look-alikes (0/O, 1/I/L) so a code read off a
// terminal is typed right the first time.
const setupCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// NewSetupCode mints the one-time first-boot setup code: 8 characters from
// setupCodeAlphabet (~39 bits), shown as XXXX-XXXX. It lives only in this
// process and the boot log; a restart mints a new one.
func NewSetupCode() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, 0, 9)
	for i, b := range raw {
		if i == 4 {
			out = append(out, '-')
		}
		// 256 % 31 leaves a slight bias; immaterial for a rate-limited,
		// single-use code.
		out = append(out, setupCodeAlphabet[int(b)%len(setupCodeAlphabet)])
	}
	return string(out), nil
}

// normalizeSetupCode makes the comparison forgiving of case, spaces and the
// display hyphen.
func normalizeSetupCode(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == ' ':
			return -1
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		}
		return r
	}, strings.TrimSpace(s))
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
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
}

// handleSetupStatus is public: the dashboard asks it on load to decide
// between the set-password page and sign-in. One indexed read, no bcrypt.
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeErr(w, http.StatusServiceUnavailable, "server_error", "dashboard authentication unavailable")
		return
	}
	first, err := s.db.DashboardFirstBoot(r.Context())
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"first_boot": first})
}

// handleSetup sets the first dashboard password. It needs the one-time setup
// code from the boot log, so whoever reaches the page first cannot claim the
// instance; failures share the login rate limit. Success flips first_boot and
// signs the caller in.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeErr(w, http.StatusServiceUnavailable, "server_error", "dashboard authentication unavailable")
		return
	}
	ip := s.sourceIP(r)
	if !loginAllowed(ip) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many attempts; try again later")
		return
	}
	var in struct {
		SetupCode string `json:"setup_code"`
		Password  string `json:"password"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	first, err := s.db.DashboardFirstBoot(r.Context())
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	if !first {
		writeErr(w, http.StatusConflict, "setup_complete", "the dashboard password is already set; sign in instead")
		return
	}
	if s.setupCode == "" {
		writeErr(w, http.StatusServiceUnavailable, "setup_unavailable",
			"no setup code was issued; restart ezllm and use the code it prints, or set EZLLM_DASHBOARD_PASSWORD")
		return
	}
	got, want := normalizeSetupCode(in.SetupCode), normalizeSetupCode(s.setupCode)
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		recordLoginFailure(ip)
		writeErr(w, http.StatusUnauthorized, "invalid_request_error", "invalid setup code")
		return
	}
	release, ok := acquireBcrypt(w, r)
	if !ok {
		return
	}
	defer release()
	if err := s.db.CompleteDashboardSetup(r.Context(), in.Password); err != nil {
		switch {
		case errors.Is(err, store.ErrDashboardPasswordTooShort):
			writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		case errors.Is(err, store.ErrDashboardSetupDone):
			writeErr(w, http.StatusConflict, "setup_complete", "the dashboard password is already set; sign in instead")
		default:
			s.writeStoreErr(w, err)
		}
		return
	}
	clearLoginAttempts(ip)
	s.log.Info("dashboard password set via first-boot setup", "ip", ip)
	token, err := s.db.DashboardLogin(r.Context(), in.Password)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	s.setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]any{"token": token})
}

func (s *Server) handleDashboardLogin(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeErr(w, http.StatusServiceUnavailable, "server_error", "dashboard authentication unavailable")
		return
	}
	ip := s.sourceIP(r)
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
	release, ok := acquireBcrypt(w, r)
	if !ok {
		return
	}
	token, err := s.db.DashboardLogin(r.Context(), in.Password)
	release()
	if err != nil {
		switch {
		case errors.Is(err, store.ErrDashboardSetupRequired):
			writeErr(w, http.StatusConflict, "setup_required", "the dashboard password has not been set; finish first-boot setup")
		case errors.Is(err, store.ErrInvalidDashboardPassword):
			// Every failure counts, empty ones included: each costs a
			// bcrypt compare.
			recordLoginFailure(ip)
			writeErr(w, http.StatusUnauthorized, "invalid_request_error", "invalid dashboard password")
		default:
			s.writeStoreErr(w, err)
		}
		return
	}
	clearLoginAttempts(ip)
	s.setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]any{"token": token})
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
	release, ok := acquireBcrypt(w, r)
	if !ok {
		return
	}
	defer release()
	if err := s.db.ChangeDashboardPassword(r.Context(), in.CurrentPassword, in.NewPassword); err != nil {
		if errors.Is(err, store.ErrInvalidDashboardPassword) {
			writeErr(w, http.StatusUnauthorized, "invalid_request_error", "current password is incorrect")
			return
		}
		if errors.Is(err, store.ErrDashboardPasswordTooShort) {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		s.writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
