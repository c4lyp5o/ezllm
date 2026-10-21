package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type closeSpy struct {
	io.Reader
	closed bool
}

func (c *closeSpy) Close() error { c.closed = true; return nil }

// The peeked prefix is replayed byte-for-byte, and closing the restored body
// still closes the upstream one (io.NopCloser used to swallow it).
func TestPeekBodyRestoresBytesAndClose(t *testing.T) {
	spy := &closeSpy{Reader: strings.NewReader(`{"error":"x"}` + strings.Repeat("y", 5000))}
	resp := &http.Response{StatusCode: 400, Body: spy}
	head := peekBody(resp, 16)
	if string(head) != `{"error":"x"}yyy` {
		t.Fatalf("peeked %q", head)
	}
	all, err := io.ReadAll(resp.Body)
	if err != nil || len(all) != 13+5000 || !strings.HasPrefix(string(all), `{"error":"x"}y`) {
		t.Fatalf("restored body: %d bytes, err %v", len(all), err)
	}
	if err := resp.Body.Close(); err != nil || !spy.closed {
		t.Fatal("closing the restored body must close the upstream body")
	}
}
