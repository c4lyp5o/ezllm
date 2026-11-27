package bridgeutil

import (
	"net/http/httptest"
	"testing"
)

// The Bearer scheme is case-insensitive, and a bare token without it is not a
// Bearer credential.
func TestAuthorizedBearerScheme(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   bool
	}{
		{"Bearer tok", true},
		{"bearer tok", true},
		{"BEARER  tok ", true},
		{"tok", false},
		{"Basic tok", false},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", tc.header)
		if got := Authorized(r, "tok", false); got != tc.want {
			t.Errorf("Authorization %q: got %v, want %v", tc.header, got, tc.want)
		}
	}
}
