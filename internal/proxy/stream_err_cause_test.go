package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// The ledger's err column has one string per call, so this rule decides what it
// says. The case that matters: a stream that was cut (the precise cause) whose
// body ALSO carries an error envelope must not lose "stream truncated: …" to a
// generic snapshot of the payload — that was the regression this pins.
func TestStreamErrCauseKeepsThePreciseCause(t *testing.T) {
	envelope := []byte(`{"error":{"message":"rate limit exceeded for this account","type":"rate_limit_exceeded"}}`)
	plainBody := []byte(`{"choices":[{"delta":{"content":"hi"}}]}`)

	cases := []struct {
		name     string
		existing string
		status   int
		first    []byte
		want     string // exact, or a substring when wantHas is set
		wantHas  string
	}{
		{
			name:     "a truncated stream keeps its cause and gains the upstream classification",
			existing: "stream truncated: unexpected EOF",
			status:   http.StatusOK,
			first:    envelope,
			wantHas:  "upstream 200 [",
		},
		{
			name:     "a truncated stream with no envelope is untouched",
			existing: "stream truncated: unexpected EOF",
			status:   http.StatusOK,
			first:    plainBody,
			want:     "stream truncated: unexpected EOF",
		},
		{
			name:    "no cause yet: the envelope becomes the record",
			status:  http.StatusTooManyRequests,
			first:   envelope,
			wantHas: "upstream 429 [",
		},
		{
			name:    "no cause, no envelope, error status: still recorded",
			status:  http.StatusBadGateway,
			first:   plainBody,
			wantHas: "upstream 502 [",
		},
		{
			name:   "no cause, no envelope, 200: nothing to say",
			status: http.StatusOK,
			first:  plainBody,
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := streamErrCause(tc.existing, tc.status, tc.first)
			if tc.wantHas != "" {
				if !strings.HasPrefix(got, tc.existing) || !strings.Contains(got, tc.wantHas) {
					t.Fatalf("streamErrCause = %q, want it to start %q and contain %q", got, tc.existing, tc.wantHas)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("streamErrCause = %q, want %q", got, tc.want)
			}
		})
	}
}

// The snippet is appended once, not once per observation: an existing value that
// already quotes the upstream must not grow a duplicate suffix.
func TestStreamErrCauseDoesNotDuplicateTheClassification(t *testing.T) {
	envelope := []byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit_exceeded"}}`)
	first := streamErrCause("stream truncated: unexpected EOF", http.StatusOK, envelope)
	second := streamErrCause(first, http.StatusOK, envelope)
	if first != second {
		t.Fatalf("re-classifying changed the record:\n first = %q\nsecond = %q", first, second)
	}
	if !strings.Contains(first, "stream truncated: unexpected EOF") || !strings.Contains(first, "rate limit exceeded") {
		t.Fatalf("the record must keep BOTH halves, got %q", first)
	}
}
