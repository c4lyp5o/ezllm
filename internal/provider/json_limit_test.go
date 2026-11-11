package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSONResponseLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		size      int
		status    int
		wantError bool
	}{
		{"boundary", 8 << 20, 200, false},
		{"oversized valid prefix", (8 << 20) + 1, 200, true},
		{"oversized auth rejection", (8 << 20) + 1, 401, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{}` + strings.Repeat(" ", tc.size-2)))
			}))
			defer srv.Close()
			body, status, err := getJSONStatus(context.Background(), srv.Client(), srv.URL, nil)
			if status != tc.status || (err != nil) != tc.wantError {
				t.Fatalf("status=%d err=%v", status, err)
			}
			if !tc.wantError && len(body) != tc.size {
				t.Fatalf("body size=%d", len(body))
			}
			if tc.status == 401 {
				he, ok := err.(*HTTPError)
				if !ok || he.Status != 401 {
					t.Fatalf("lost auth classification: %v", err)
				}
			}
		})
	}
}
