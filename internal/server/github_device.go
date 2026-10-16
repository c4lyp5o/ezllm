package server

// Dashboard GitHub device flow — the browser replaces `ezllm github-login`'s
// terminal. The division of labour is exactly the CLI's, with the operator's
// phone standing in for the operator's laptop:
//
//	POST /admin/github/device            → request the device code (server)
//	GET  /admin/github/device/{handle}   → one token poll per tick (server)
//	                                   …the human taps Authorize (browser)
//	POST /admin/accounts/{id}/keys       → device_handle resolves the session
//
// Every secret step stays server-side. The OAuth session and the minted
// Copilot token are held in process memory under the handle and leave ONLY
// through the key-add path, which consumes them — no response body ever
// carries them (the CLI printed the JSON to a terminal; a dashboard would be
// shipping account credentials to a browser and into proxy logs).
//
// No schema: the registry is in-memory and bounded by ghDeviceTTL, so an
// abandoned flow costs one map entry for 30 minutes and then evaporates.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/githubcopilot"
)

// ghDeviceTTL bounds how long an entry survives. GitHub's own code window is
// 15 minutes; the grace after that is the time an authorized operator needs
// to click Save & validate. Past this the handle 404s and the session — if
// one was minted — is dropped from memory.
const ghDeviceTTL = 30 * time.Minute

// ghDevice is one in-flight dashboard device flow. Fields are guarded by
// Server.ghMu: the status handler snapshots, talks to GitHub WITHOUT the
// lock held, then writes back under it.
type ghDevice struct {
	dc      githubcopilot.DeviceCode
	created time.Time
	session string // encoded Session JSON — server-side only, never a response
	login   string
	expiry  time.Time // minted Copilot token expiry, for the status display
	// failure is terminal: GitHub denied the grant or the account cannot
	// mint a Copilot token. It rides key adds so a doomed handle cannot
	// store anything, and status GETs so the UI can say why.
	failure string
}

// ghDeviceStatus is the wire shape of GET /admin/github/device/{handle}.
// Deliberately session-free: knowing the login is proof the tap landed, and
// that is all the dashboard needs before it submits the key add.
type ghDeviceStatus struct {
	Status           string `json:"status"` // pending | authorized | expired | error
	GitHubLogin      string `json:"github_login,omitempty"`
	CopilotExpiresAt string `json:"copilot_expires_at,omitempty"`
	Detail           string `json:"detail,omitempty"`
}

// handleGitHubDeviceStart starts a flow: ask GitHub for a device code and
// hand the dashboard a handle to poll. Prunes dead entries first so a
// forgetful operator cannot grow the map without bound.
func (s *Server) handleGitHubDeviceStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	s.pruneGHDevices()
	dc, err := githubcopilot.RequestDeviceCode(r.Context(), s.ghCfg)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "api_error",
			fmt.Sprintf("GitHub device code request failed: %v", err))
		return
	}
	handle, err := newGHHandle()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "server_error", "could not allocate a device handle")
		return
	}
	s.ghMu.Lock()
	s.ghDevices[handle] = &ghDevice{dc: dc, created: time.Now()}
	s.ghMu.Unlock()
	if dc.ExpiresIn == 0 {
		dc.ExpiresIn = 15 * 60
	}
	if dc.Interval <= 0 {
		dc.Interval = 5
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"handle":           handle,
		"verification_uri": dc.VerificationURI,
		"user_code":        dc.UserCode,
		"expires_in":       dc.ExpiresIn,
		"interval":         dc.Interval,
	})
}

// handleGitHubDeviceStatus answers ONE poll per tick: pending, authorized
// (mint verified, login captured), expired or error. GitHub is contacted
// without the registry lock — a slow upstream must not block admin traffic.
func (s *Server) handleGitHubDeviceStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	handle := r.PathValue("handle")

	s.ghMu.Lock()
	dev, ok := s.ghDevices[handle]
	if !ok {
		s.ghMu.Unlock()
		writeErr(w, http.StatusNotFound, "not_found", "unknown or expired device handle — start the GitHub login again")
		return
	}
	// Snapshot: the network call below happens unlocked, and another tick /
	// key add may write these fields concurrently.
	dc, created := dev.dc, dev.created
	session, login, expiry, failure := dev.session, dev.login, dev.expiry, dev.failure
	s.ghMu.Unlock()

	switch {
	case failure != "":
		writeJSON(w, http.StatusOK, ghDeviceStatus{Status: "error", Detail: failure})
	case session != "":
		st := ghDeviceStatus{Status: "authorized", GitHubLogin: login}
		if !expiry.IsZero() {
			st.CopilotExpiresAt = expiry.Format(time.RFC3339)
		}
		writeJSON(w, http.StatusOK, st)
	case ghCodeExpired(dc, created):
		s.dropGHDevice(handle)
		writeJSON(w, http.StatusOK, ghDeviceStatus{Status: "expired"})
	default:
		s.pollGitHubDevice(w, r, handle, dc)
	}
}

// pollGitHubDevice is the unlocked GitHub contact: one token poll, and on a
// grant the mint + identity read the CLI used to do in the same breath.
func (s *Server) pollGitHubDevice(w http.ResponseWriter, r *http.Request, handle string, dc githubcopilot.DeviceCode) {
	ctx := r.Context()
	sess, granted, err := githubcopilot.PollAccessTokenOnce(ctx, s.ghCfg, dc)
	if err != nil {
		if isTerminalDeviceErr(err) {
			// GitHub itself refused (denied / expired_grant / bad client):
			// this code is dead, say so and pin the reason on the handle.
			msg := err.Error()
			s.setGHFailure(handle, msg)
			writeJSON(w, http.StatusOK, ghDeviceStatus{Status: "error", Detail: msg})
			return
		}
		// Transport hiccup on a still-live code: report pending with the
		// reason so the UI keeps ticking; the next poll retries.
		writeJSON(w, http.StatusOK, ghDeviceStatus{Status: "pending", Detail: err.Error()})
		return
	}
	if !granted {
		writeJSON(w, http.StatusOK, ghDeviceStatus{Status: "pending"})
		return
	}

	// Authorized. Mint now, exactly like the CLI does: a token that cannot
	// mint is not an account this router can serve, and this is the earliest
	// honest failure point (entitlement lives on GitHub's side).
	token, exp, err := githubcopilot.MintCopilotToken(ctx, s.ghCfg, sess.AccessToken)
	if err != nil {
		msg := fmt.Sprintf("copilot token mint failed (is the account entitled to Copilot?): %v", err)
		s.setGHFailure(handle, msg)
		writeJSON(w, http.StatusOK, ghDeviceStatus{Status: "error", Detail: msg})
		return
	}
	sess.CopilotToken, sess.CopilotExpiresAt = token, exp

	identity := ""
	if info, err := githubcopilot.FetchUserInfo(ctx, s.ghCfg, sess.AccessToken); err == nil {
		identity = info.Login
	}

	s.ghMu.Lock()
	if dev, ok := s.ghDevices[handle]; ok {
		dev.session, dev.login, dev.expiry = sess.Encode(), identity, exp
	}
	s.ghMu.Unlock()

	writeJSON(w, http.StatusOK, ghDeviceStatus{
		Status: "authorized", GitHubLogin: identity, CopilotExpiresAt: exp.Format(time.RFC3339),
	})
}

// handleGitHubDeviceCancel drops an abandoned flow (modal closed before the
// tap). Best-effort: a missing handle is already the desired end state.
func (s *Server) handleGitHubDeviceCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	s.dropGHDevice(r.PathValue("handle"))
	w.WriteHeader(http.StatusNoContent)
}

// ghSession hands the stored session to the key-add path. It does NOT
// consume: if the registration test fails, the operator must be free to hit
// Save again with the same authorization. Returns (session, 0, "") on
// success, or ("", httpStatus, message) — the refusal to surface as a 4xx.
func (s *Server) ghSession(handle string) (string, int, string) {
	s.ghMu.Lock()
	defer s.ghMu.Unlock()
	dev, ok := s.ghDevices[handle]
	if !ok {
		return "", http.StatusNotFound, "unknown or expired device handle — start the GitHub login again"
	}
	switch {
	case dev.failure != "":
		return "", http.StatusConflict, dev.failure
	case dev.session != "":
		return dev.session, 0, ""
	case ghCodeExpired(dev.dc, dev.created):
		delete(s.ghDevices, handle)
		return "", http.StatusGone, "device code expired before authorization — start the GitHub login again"
	default:
		return "", http.StatusConflict, "not authorized yet — finish authorizing at github.com/login/device"
	}
}

// ghConsume retires a handle after a successful key store: the credential
// now lives in the encrypted key store, and a second POST of the same
// device_handle must not mint a duplicate row from a stale copy.
func (s *Server) ghConsume(handle string) { s.dropGHDevice(handle) }

func (s *Server) dropGHDevice(handle string) {
	s.ghMu.Lock()
	delete(s.ghDevices, handle)
	s.ghMu.Unlock()
}

func (s *Server) setGHFailure(handle, msg string) {
	s.ghMu.Lock()
	if dev, ok := s.ghDevices[handle]; ok {
		dev.failure = msg
	}
	s.ghMu.Unlock()
}

// pruneGHDevices enforces ghDeviceTTL and GitHub's own code expiry on the
// pre-auth entries. Called on every start, the only moment new memory can
// appear — no ticker to leak, no goroutine to outlive the request.
func (s *Server) pruneGHDevices() {
	s.ghMu.Lock()
	defer s.ghMu.Unlock()
	for h, d := range s.ghDevices {
		switch {
		case time.Since(d.created) > ghDeviceTTL:
			delete(s.ghDevices, h)
		case d.session == "" && d.failure == "" && ghCodeExpired(d.dc, d.created):
			delete(s.ghDevices, h)
		}
	}
}

// ghCodeExpired tracks GitHub's expires_in against the moment we asked.
func ghCodeExpired(dc githubcopilot.DeviceCode, created time.Time) bool {
	seconds := dc.ExpiresIn
	if seconds <= 0 {
		seconds = 15 * 60
	}
	return time.Since(created) > time.Duration(seconds)*time.Second
}

// isTerminalDeviceErr separates "GitHub said no" from "the network failed
// us": a refusal (denied, expired_grant, bad client) kills the flow, a
// transport blip must not eat a code the operator may still be approving.
func isTerminalDeviceErr(err error) bool {
	msg := err.Error()
	for _, transient := range []string{
		"connection refused", "no such host", "i/o timeout", "timeout",
		"EOF", "reset by peer", "Temporary failure", "connection reset",
	} {
		if strings.Contains(msg, transient) {
			return false
		}
	}
	return true
}

// newGHHandle is a 128-bit random handle: unguessable, since knowing one is
// the difference between polling a flow and stealing its session at key-add.
func newGHHandle() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
