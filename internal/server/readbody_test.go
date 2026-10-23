package server

import (
	"bytes"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadBody(t *testing.T) {
	const max = 64
	for _, tc := range []struct {
		name     string
		body     string
		cl       int64 // -1 = unknown (chunked)
		want     string
		tooLarge bool
		wantErr  bool
	}{
		{"known length", "hello", 5, "hello", false, false},
		{"known length at the cap", strings.Repeat("a", max), max, strings.Repeat("a", max), false, false},
		{"declared over the cap", "small", max + 1, "", true, false},
		{"chunked under the cap", "chunk", -1, "chunk", false, false},
		{"chunked over the cap", strings.Repeat("b", max+1), -1, "", true, false},
		{"empty", "", 0, "", false, false},
		{"shorter than declared", "abc", 10, "", false, true},
		{"longer than declared stays capped", strings.Repeat("c", max+5), 3, "", true, false},
		{"longer than declared under the cap", "abcdef", 3, "abcdef", false, false},
	} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Body = io.NopCloser(bytes.NewReader([]byte(tc.body)))
		r.ContentLength = tc.cl
		got, tooLarge, err := readBody(r, max)
		if (err != nil) != tc.wantErr || tooLarge != tc.tooLarge {
			t.Errorf("%s: tooLarge=%v err=%v, want tooLarge=%v err=%v", tc.name, tooLarge, err, tc.tooLarge, tc.wantErr)
			continue
		}
		if !tc.wantErr && !tc.tooLarge && string(got) != tc.want {
			t.Errorf("%s: body %q, want %q", tc.name, got, tc.want)
		}
	}
}
