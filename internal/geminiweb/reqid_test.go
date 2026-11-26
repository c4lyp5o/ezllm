package geminiweb

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A long-lived client keeps a positive _reqid past the int32 range (it went
// negative after about 21,000 requests).
func TestReqIDStaysPositivePastInt32(t *testing.T) {
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc(endpointInit, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, initPageFixture) })
	mux.HandleFunc(endpointGenerate, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, genPart(nil, []any{candidate("rcid-1", "ok", "", true)}, "final"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := initGenerateClient(t, srv)
	c.reqID = math.MaxInt32 - 50000
	for i := 0; i < 2; i++ {
		if _, err := c.Generate(context.Background(), GenerateOptions{Model: testModel(), Prompt: "hi"}, func(Delta) {}); err != nil {
			t.Fatalf("generate: %v", err)
		}
	}
	want := fmt.Sprintf("_reqid=%d", int64(math.MaxInt32)+50000)
	if !strings.Contains(gotQuery, want) {
		t.Fatalf("query %q, want %s", gotQuery, want)
	}
}
