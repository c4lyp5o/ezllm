package claudeweb

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Timeout bounds the wait for headers only: a completion that streams past
// it is read to the end, not cut mid-sentence.
func TestTLSFetcherTimeoutSparesTheStreamingBody(t *testing.T) {
	srv, f, _ := h2Server(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first ")
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond)
		io.WriteString(w, "last")
	})
	f.Timeout = 100 * time.Millisecond
	resp, err := f.Fetch(context.Background(), http.MethodGet, srv.URL+"/stream", nil, http.Header{})
	if err != nil {
		t.Skipf("local TLS handshake with the chrome profile failed (%v)", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || string(b) != "first last" {
		t.Fatalf("body = %q, err = %v; want the whole stream past the header timeout", b, err)
	}
}

// The header wait itself is still bounded.
func TestTLSFetcherTimeoutStillBoundsHeaders(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv, f, _ := h2Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warm" {
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	if resp, err := f.Fetch(context.Background(), http.MethodGet, srv.URL+"/warm", nil, http.Header{}); err != nil {
		t.Skipf("local TLS handshake with the chrome profile failed (%v)", err)
	} else {
		resp.Body.Close()
	}
	f.Timeout = 100 * time.Millisecond
	start := time.Now()
	_, err := f.Fetch(context.Background(), http.MethodGet, srv.URL+"/hang", nil, http.Header{})
	if err == nil {
		t.Fatal("fetch with no response headers succeeded; want a timeout")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("header timeout took %s", d)
	}
}

// A tool call in the answer fails the turn by name instead of ending as an
// empty "stop".
func TestConsumeSSEToolCallFailsTheTurn(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"uuid":"a1"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"get_weather","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n")
	var snip []byte
	var assist string
	_, completed, err := consumeSSE(strings.NewReader(stream), func(string) {}, &snip, &assist)
	if !errors.Is(err, ErrToolCall) || completed {
		t.Fatalf("completed = %v, err = %v; want ErrToolCall", completed, err)
	}
	if !strings.Contains(err.Error(), "chat_tools_unsupported") {
		t.Fatalf("error %q must name chat_tools_unsupported so a combo moves on", err)
	}
}

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

// deflate is advertised in Accept-Encoding, so it must decode.
func TestDecodeBodyDeflate(t *testing.T) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	io.WriteString(zw, `{"ok":true}`)
	zw.Close()
	resp := &http.Response{Header: http.Header{"Content-Encoding": {"deflate"}}, Body: io.NopCloser(&buf)}
	rc, err := decodeBody(resp)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != `{"ok":true}` {
		t.Fatalf("deflate body = %q", b)
	}
}

// Closing a zstd body closes the decoder and the response body.
func TestDecodeBodyZstdClosesBoth(t *testing.T) {
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	io.WriteString(zw, "hello")
	zw.Close()
	body := &closeRecorder{Reader: &buf}
	rc, err := decodeBody(&http.Response{Header: http.Header{"Content-Encoding": {"zstd"}}, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(rc); string(b) != "hello" {
		t.Fatalf("zstd body = %q", b)
	}
	rc.Close()
	if !body.closed {
		t.Fatal("response body not closed")
	}
	d, ok := rc.(*decodedBody)
	if !ok || d.closeDecoder == nil {
		t.Fatalf("zstd body %T has no decoder close", rc)
	}
}

// An account in several organizations uses the one the browser marks active.
func TestOrganizationIDPrefersLastActiveOrg(t *testing.T) {
	mf := &mockFetcher{orgs: func(w http.ResponseWriter) {
		fmt.Fprint(w, `[{"uuid":"org-free"},{"uuid":"org-pro"}]`)
	}}
	c := newMock(t, mf)
	c.cookie = "sessionKey=sk-ant-test; lastActiveOrg=org-pro"
	id, err := c.OrganizationID(context.Background())
	if err != nil || id != "org-pro" {
		t.Fatalf("org = %q, %v; want org-pro from lastActiveOrg", id, err)
	}
}

func TestSnippetKeepsUTF8(t *testing.T) {
	s := snippet([]byte(strings.Repeat("a", 299) + "é" + strings.Repeat("b", 10)))
	if !strings.HasSuffix(s, "…") || strings.ContainsRune(s, '�') || !utfValid(s) {
		t.Fatalf("snippet split a rune: %q", s)
	}
}

func utfValid(s string) bool { return strings.ToValidUTF8(s, "") == s }

func TestHostTimezoneNeverSendsLocal(t *testing.T) {
	if got := hostTimezone(time.FixedZone("Local", 0)); got != "UTC" {
		t.Fatalf("timezone = %q, want UTC for Go's unnamed local zone", got)
	}
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	if got := hostTimezone(loc); got != "Europe/Berlin" {
		t.Fatalf("timezone = %q", got)
	}
}
