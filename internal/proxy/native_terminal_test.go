package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestNativeStreamRequiresTerminalEvent(t *testing.T) {
	cases := []struct {
		name     string
		surface  provider.Surface
		body     string
		complete bool
	}{
		{"chat cut", provider.SurfaceChat, "data: {\"choices\":[]}\n\n", false},
		{"chat done", provider.SurfaceChat, "data: [DONE]\n\n", true},
		{"messages cut", provider.SurfaceAnthropic, "data: {\"type\":\"message_delta\"}\n\n", false},
		{"messages stop", provider.SurfaceAnthropic, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", true},
		{"responses completed", provider.SurfaceResponses, "data: {\"type\":\"response.completed\"}\n\n", true},
		{"responses incomplete", provider.SurfaceResponses, "data: {\"type\":\"response.incomplete\"}\n\n", true},
		{"responses cut", provider.SurfaceResponses, "data: {\"type\":\"response.output_text.delta\"}\n\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}
			resp.Body = httpBody(tc.body)
			rec := httptest.NewRecorder()
			_, _, _, err := newDispatcher().streamCopyTracked(context.Background(), rec, resp, tc.surface, provider.UsageConventionAuto, time.Now(), true)
			if (err == nil) != tc.complete {
				t.Fatalf("error = %v, complete = %v", err, tc.complete)
			}
			if rec.Body.String() != tc.body {
				t.Fatalf("stream changed: %q", rec.Body.String())
			}
		})
	}
}

func httpBody(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }

func TestNativeCleanEOFCutMarksDispatchTruncated(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer up.Close()
	req, body := reqWithBody(`{"model":"day","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	_, res, err := newDispatcher().ForwardCandidates(context.Background(), rec, req, []Route{textRoute(up.URL, "day")}, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StreamOutcome != StreamOutcomeTruncated {
		t.Fatalf("stream outcome = %q, want truncated", res.StreamOutcome)
	}
}
