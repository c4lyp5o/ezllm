package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// signalWriter records what reaches the client and signals the first byte.
type signalWriter struct {
	mu     sync.Mutex
	header http.Header
	body   bytes.Buffer
	status int
	first  chan struct{}
	once   sync.Once
}

func (s *signalWriter) Header() http.Header { return s.header }
func (s *signalWriter) WriteHeader(c int)   { s.status = c }
func (s *signalWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.once.Do(func() { close(s.first) })
	return s.body.Write(p)
}
func (s *signalWriter) Flush() {}

// Speech audio reaches the client while the upstream is still generating.
func TestForwardStreamsAudioAsItArrives(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("ID3-first-chunk"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
		w.Write([]byte("-rest"))
	}))
	defer upstream.Close()

	d := newDispatcher()
	body := []byte(`{"model":"resolved-model","input":"hello","voice":"alloy"}`)
	sw := &signalWriter{header: http.Header{}, first: make(chan struct{})}
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	done := make(chan Result, 1)
	go func() {
		res, _ := d.Forward(context.Background(), sw, req, testRoute(upstream.URL, provider.SurfaceAudioTTS), body)
		done <- res
	}()
	select {
	case <-sw.first:
	case <-time.After(2 * time.Second):
		close(release)
		<-done
		t.Fatal("no audio reached the client before the upstream finished: the body was buffered")
	}
	close(release)
	res := <-done
	if got := sw.body.String(); got != "ID3-first-chunk-rest" || res.Err != "" || res.Status != 200 {
		t.Fatalf("body %q, err %q, status %d", got, res.Err, res.Status)
	}
	if ct := sw.header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("content type %q", ct)
	}
}

// An upstream that dies mid-file is recorded as truncated.
func TestForwardRecordsTruncatedAudio(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("RIFF-partial"))
	}))
	defer upstream.Close()
	d := newDispatcher()
	body := []byte(`{"model":"resolved-model","input":"hello"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	res, _ := d.Forward(context.Background(), rec, req, testRoute(upstream.URL, provider.SurfaceAudioTTS), body)
	if !strings.HasPrefix(res.Err, "stream truncated") || res.StreamCompleted() {
		t.Fatalf("err %q outcome %q, want a truncated delivery", res.Err, res.StreamOutcome)
	}
}

func TestIsBinaryMedia(t *testing.T) {
	for ct, want := range map[string]bool{
		"audio/mpeg": true, "audio/wav; codecs=1": true, "video/mp4": true, "image/png": true,
		"application/octet-stream": true, "application/json": false, "text/event-stream": false, "": false,
	} {
		if got := isBinaryMedia(ct); got != want {
			t.Errorf("%q: %v, want %v", ct, got, want)
		}
	}
}
