package provider

import (
	"net/http"
	"testing"
)

// The classifier decides whether one probe response means "this model cannot
// speak this protocol" (record proto_*=0), "we could not ask" (record NULL), or
// "the credential died" (abort the sweep). Erring either way is costly: too
// eager records a live model as unsupported and drops it from routing; too lax
// leaves a real capability gap recorded as unknown, so the router keeps dialling
// it and the CLIENT keeps eating the upstream's refusal.
func TestClassifyProbeResultCases(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		want         ProbeVerdict
		wantAuthDead bool
	}{
		{
			name:   "2xx is supported",
			status: http.StatusOK,
			body:   `{"id":"msg_1","type":"message"}`,
			want:   VerdictSupported,
		},
		{
			// LIVE 2026-10-11: space-stationine's group refuses the whole
			// surface, and the refusal carries none of the words a 400 gap
			// carries. Read as a dead key it would abort the sweep and record
			// nothing, leaving the gap invisible forever.
			name:   "403 group policy naming the surface is a capability gap",
			status: http.StatusForbidden,
			body:   `{"error":{"message":"This group does not allow /v1/messages dispatch","type":"permission_error"},"type":"error"}`,
			want:   VerdictUnsupported,
		},
		{
			name:         "403 about the credential stays a dead key",
			status:       http.StatusForbidden,
			body:         `{"error":{"message":"invalid api key","type":"authentication_error"}}`,
			want:         VerdictUnknown,
			wantAuthDead: true,
		},
		{
			// A 401 is never a capability gap, even when its body happens to
			// use the word "unsupported" — an auth failure must still abort.
			name:         "401 is always a dead key",
			status:       http.StatusUnauthorized,
			body:         `{"error":{"message":"protocol unsupported for this key"}}`,
			want:         VerdictUnknown,
			wantAuthDead: true,
		},
		{
			// LIVE 2026-10-11: opencode-go /v1/messages on step-5-preview-free.
			name:   "400 ModelProtocolUnsupported",
			status: http.StatusBadRequest,
			body:   `{"type":"error","error":{"type":"ModelProtocolUnsupported","message":"Model does not support this protocol."}}`,
			want:   VerdictUnsupported,
		},
		{
			// A plain bad-request must NOT be recorded: guessing 0 here would
			// push a working model out of the combo.
			name:   "unrecognised 400 records nothing",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"context length exceeded"}}`,
			want:   VerdictUnknown,
		},
		{
			name:   "429 is unknown, never unsupported",
			status: http.StatusTooManyRequests,
			body:   `{"error":"slow down"}`,
			want:   VerdictUnknown,
		},
		{
			name:   "missing surface endpoint means the kind cannot serve it",
			status: http.StatusNotFound,
			body:   `{"error":"not found"}`,
			want:   VerdictUnsupported,
		},
		{
			name:   "5xx is unknown",
			status: http.StatusBadGateway,
			body:   `<html>bad gateway</html>`,
			want:   VerdictUnknown,
		},
	}
	for _, c := range cases {
		got, authDead := ClassifyProbeResult(c.status, c.body)
		if got != c.want || authDead != c.wantAuthDead {
			t.Errorf("%s: ClassifyProbeResult(%d) = (%v,%v), want (%v,%v)",
				c.name, c.status, got, authDead, c.want, c.wantAuthDead)
		}
	}
}
