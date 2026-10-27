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

func TestAccountingMultilineSSE(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		source := strings.Join([]string{": keepalive", "data: {\"choices\":[],", "data: \"usage\":{\"prompt_tokens\":100,\"completion_tokens\":42}}", "", "data: [DONE]", "", ""}, newline)
		rec := httptest.NewRecorder()
		resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(source)), Header: make(http.Header)}
		_, u, _, err := newDispatcher().streamCopy(context.Background(), rec, resp, provider.SurfaceChat, provider.UsageConventionAuto, time.Now())
		if err != nil || rec.Body.String() != source {
			t.Fatalf("copy changed: %v", err)
		}
		if u == nil || u.In != 100 || u.Out != 42 {
			t.Fatalf("multiline usage lost: %+v", u)
		}
	}
}

func TestAccountingSSEBoundAndRecovery(t *testing.T) {
	tp := NewUsageTapperFor(provider.SurfaceChat, provider.UsageConventionAuto)
	tp.Observe([]byte("data: {"))
	tp.Observe([]byte("data: " + strings.Repeat(" ", maxScanToken)))
	tp.Observe([]byte(`data: "usage":{"prompt_tokens":999}}`))
	if tp.Usage() != nil {
		t.Fatal("oversized event counted")
	}
	tp.Observe(nil)
	tp.Observe([]byte(`data: {"usage":{"prompt_tokens":7,"completion_tokens":2}}`))
	if u := tp.Usage(); u == nil || u.In != 7 || u.Out != 2 {
		t.Fatalf("tap did not recover: %+v", u)
	}
}
