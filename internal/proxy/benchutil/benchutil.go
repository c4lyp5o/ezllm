// Package benchutil is TEST-ONLY support code: the deterministic local mock
// upstream that ezllm's benchmarks, latency-percentile harness and lifecycle
// tests (cancellation, slow consumer, retained memory) drive the real request
// path against. Nothing in the production binary imports it — it exists so that
// every measurement in docs/perf-baseline-m1.md is reproducible against one
// upstream implementation instead of a per-package ad-hoc stub.
//
// It is a normal package rather than a `_test.go` file for one Go reason: a
// package made only of `_test.go` files cannot be imported by another package's
// tests, and the same mock has to serve internal/proxy and internal/server.
//
// Every byte it emits is fixed at construction time. There is no randomness, no
// clock dependence in the payload, no real network and no real provider: an
// httptest.Server on a loopback ephemeral port is the only listener. (Never
// bind 20129 — the production gateway is live there.)
package benchutil

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options describes one deterministic upstream behaviour. Zero values are
// filled by withDefaults, so Options{} is a valid small non-streaming mock.
type Options struct {
	// ResponseBytes is the content length of the NON-stream JSON body's
	// assistant message. The rest of the envelope is fixed.
	ResponseBytes int
	// Chunks is the number of SSE data events before the terminal usage event.
	Chunks int
	// ChunkBytes is the content length carried by each SSE data event.
	ChunkBytes int
	// Fragments is how many Write+Flush calls each SSE frame is split into.
	// 1 = the whole frame in one write. Values > 1 split the frame on arbitrary
	// byte boundaries — including INSIDE `data: {...}` — which is the
	// fragmented-event case a real network can always produce and which a
	// buffering implementation would silently paper over.
	Fragments int
	// BodyPieces is how many Write calls the NON-stream JSON body is split
	// into. > 1 makes the body arrive in instalments, which is what the
	// "client disconnects mid non-stream read" test needs.
	BodyPieces int
	// Delay is waited before each write after the first (stream chunks and
	// non-stream body pieces alike). Zero means "as fast as the socket takes
	// it". It is what makes a slow stream slow without any Sleep in the test.
	Delay time.Duration
	// Status is the response status. Zero means 200. A value >= 400 makes the
	// mock answer ErrorBody instead of a chat completion.
	Status int
	// ErrorBody is the body sent when Status >= 400.
	ErrorBody string
}

func (o Options) withDefaults() Options {
	if o.ResponseBytes <= 0 {
		o.ResponseBytes = 1024
	}
	if o.Chunks <= 0 {
		o.Chunks = 16
	}
	if o.ChunkBytes <= 0 {
		o.ChunkBytes = 64
	}
	if o.Fragments <= 0 {
		o.Fragments = 1
	}
	if o.BodyPieces <= 0 {
		o.BodyPieces = 1
	}
	if o.Status == 0 {
		o.Status = http.StatusOK
	}
	return o
}

// Request is one recorded upstream call. Bodies and headers are kept verbatim
// for assertions; nothing here is ever logged by the mock.
type Request struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// Mock is a deterministic OpenAI-compatible upstream. It implements
// http.Handler, so a caller that wants to own the listener can hand
// m.ServeHTTP to httptest.NewServer; New starts one for callers that do not.
type Mock struct {
	opts Options

	srv *httptest.Server

	jsonBody []byte   // non-stream response, fixed at construction
	frames   [][]byte // SSE frames: Chunks data events + terminal usage + [DONE]

	mu       sync.Mutex
	requests []Request

	written int64 // bytes handed to the ResponseWriter (backpressure probe)

	canceled   chan struct{}
	cancelOnce sync.Once
	cancels    int32

	firstWrite     chan struct{}
	firstWriteOnce sync.Once
}

// New builds a Mock and starts its httptest.Server on a loopback ephemeral
// port. Close it when done.
func New(opts Options) *Mock {
	m := NewHandler(opts)
	m.srv = httptest.NewServer(m)
	return m
}

// NewHandler builds a Mock WITHOUT starting a server: it is a plain
// http.Handler for a harness that already owns (or deliberately wraps) the
// listener. The recorded requests, cancellation signal and byte counter work
// identically.
func NewHandler(opts Options) *Mock {
	o := opts.withDefaults()
	m := &Mock{
		opts:       o,
		canceled:   make(chan struct{}),
		firstWrite: make(chan struct{}),
	}
	if o.Status >= 400 {
		m.jsonBody = []byte(o.ErrorBody)
		return m
	}
	m.jsonBody = completionBody(o.ResponseBytes)
	m.frames = make([][]byte, 0, o.Chunks+2)
	for i := 0; i < o.Chunks; i++ {
		m.frames = append(m.frames, chunkFrame(i, o.ChunkBytes))
	}
	m.frames = append(m.frames, terminalFrame(), []byte("data: [DONE]\n\n"))
	return m
}

// URL is the mock's base URL. It is "" for a NewHandler mock.
func (m *Mock) URL() string {
	if m.srv == nil {
		return ""
	}
	return m.srv.URL
}

// Server exposes the underlying httptest.Server (nil for a NewHandler mock), so
// a harness that already has one can adopt it instead of wrapping the handler
// in a second listener.
func (m *Mock) Server() *httptest.Server { return m.srv }

// Close shuts the server down. Safe to call twice and safe on a NewHandler
// mock.
func (m *Mock) Close() {
	if m.srv != nil {
		m.srv.Close()
	}
}

// Options returns the effective (defaulted) options.
func (m *Mock) Options() Options { return m.opts }

// Hits is the number of requests the mock has served.
func (m *Mock) Hits() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// Requests returns a copy of the recorded requests, in arrival order.
func (m *Mock) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Request, len(m.requests))
	copy(out, m.requests)
	return out
}

// BytesWritten is the number of response bytes the mock has handed to its
// ResponseWriter. A stalled value while the client is not reading is the
// evidence that TCP backpressure propagated all the way upstream.
func (m *Mock) BytesWritten() int64 { return atomic.LoadInt64(&m.written) }

// StreamPayload is the exact SSE byte stream the mock would write for a
// streaming request: Chunks data events, the terminal usage event and the
// [DONE] sentinel, concatenated. Benchmarks that measure the forwarding loop in
// isolation feed this to the copier, so the bytes under test are the mock's own
// rather than a second hand-written fixture.
func (m *Mock) StreamPayload() []byte {
	var b bytes.Buffer
	for _, f := range m.frames {
		b.Write(f)
	}
	return b.Bytes()
}

// Canceled is closed the first time the mock OBSERVES, from inside a running
// handler, that its request context has been canceled.
//
// The qualifier carries the whole weight of the assertion, so it is enforced
// structurally rather than by a heuristic. net/http cancels a request's context
// as a matter of course once the handler RETURNS, so a watcher goroutine
// listening on r.Context().Done() would fire on every completed request and a
// cancellation test built on it would pass vacuously. This mock therefore has
// no watcher goroutine at all: it reports a cancellation only at the two points
// where a RUNNING handler discovers one — a delay select that loses to
// r.Context().Done(), and a write that failed while the context was already
// canceled. A handler that ran to completion reaches neither.
func (m *Mock) Canceled() <-chan struct{} { return m.canceled }

// CancelCount is how many upstream requests were abandoned mid-handler.
func (m *Mock) CancelCount() int { return int(atomic.LoadInt32(&m.cancels)) }

// WroteFirst is closed once the mock has handed its first response byte to the
// ResponseWriter. A test uses it as a synchronization point — "the response has
// demonstrably started" — instead of sleeping and hoping.
func (m *Mock) WroteFirst() <-chan struct{} { return m.firstWrite }

// Reset clears the recorded requests, the byte counter and the cancellation
// signal, so one mock can serve several independent assertions.
func (m *Mock) Reset() {
	m.mu.Lock()
	m.requests = nil
	m.mu.Unlock()
	atomic.StoreInt64(&m.written, 0)
	atomic.StoreInt32(&m.cancels, 0)
	m.cancelOnce = sync.Once{}
	m.canceled = make(chan struct{})
	m.firstWriteOnce = sync.Once{}
	m.firstWrite = make(chan struct{})
}

// ServeHTTP implements http.Handler.
func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	m.mu.Lock()
	m.requests = append(m.requests, Request{
		Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body,
	})
	m.mu.Unlock()

	if m.opts.Status >= 400 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(m.opts.Status)
		m.writePieces(w, r, []byte(m.opts.ErrorBody))
		return
	}

	var probe struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.Stream {
		m.serveStream(w, r)
		return
	}
	m.serveJSON(w, r)
}

func (m *Mock) serveStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	if f != nil {
		f.Flush()
	}
	for _, frame := range m.frames {
		if !m.wait(r) {
			return
		}
		if !m.writeFragmented(w, r, f, frame) {
			return
		}
	}
}

func (m *Mock) serveJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(m.jsonBody)))
	w.WriteHeader(http.StatusOK)
	m.writePieces(w, r, m.jsonBody)
}

// writePieces writes body in BodyPieces instalments, flushing after each so a
// reader sees them arrive separately rather than in one buffered blob.
func (m *Mock) writePieces(w http.ResponseWriter, r *http.Request, body []byte) {
	f, _ := w.(http.Flusher)
	step := (len(body) + m.opts.BodyPieces - 1) / m.opts.BodyPieces
	if step < 1 {
		step = len(body)
	}
	for off := 0; off < len(body); off += step {
		if off > 0 && !m.wait(r) {
			return
		}
		end := off + step
		if end > len(body) {
			end = len(body)
		}
		if !m.write(w, r, f, body[off:end]) {
			return
		}
	}
}

// wait honours Delay, and reports false when the request context ended first so
// the handler stops promptly instead of writing into a dead connection.
func (m *Mock) wait(r *http.Request) bool {
	if m.opts.Delay <= 0 {
		return true
	}
	t := time.NewTimer(m.opts.Delay)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		m.reportCancel(r)
		return false
	}
}

// writeFragmented writes one SSE frame, split into Fragments Write+Flush pairs.
// The split is byte-wise and deliberately unaware of frame structure, so a
// fragmented mock really does cut `data: {...}` in half.
func (m *Mock) writeFragmented(w http.ResponseWriter, r *http.Request, f http.Flusher, frame []byte) bool {
	n := m.opts.Fragments
	if n <= 1 {
		return m.write(w, r, f, frame)
	}
	step := (len(frame) + n - 1) / n
	for off := 0; off < len(frame); off += step {
		end := off + step
		if end > len(frame) {
			end = len(frame)
		}
		if !m.write(w, r, f, frame[off:end]) {
			return false
		}
	}
	return true
}

func (m *Mock) write(w http.ResponseWriter, r *http.Request, f http.Flusher, p []byte) bool {
	if _, err := w.Write(p); err != nil {
		// The connection is gone. Attribute it to cancellation only when the
		// context really was canceled — a peer can also just close.
		m.reportCancel(r)
		return false
	}
	atomic.AddInt64(&m.written, int64(len(p)))
	m.firstWriteOnce.Do(func() { close(m.firstWrite) })
	if f != nil {
		f.Flush()
	}
	return true
}

// reportCancel records a cancellation the handler observed for itself. It is
// idempotent and never blocks.
func (m *Mock) reportCancel(r *http.Request) {
	if r.Context().Err() == nil {
		return
	}
	atomic.AddInt32(&m.cancels, 1)
	m.cancelOnce.Do(func() { close(m.canceled) })
}

// NewClient returns an http.Client tuned for benchmark round trips: keep-alive
// with a large idle pool, so the measured time is the request rather than a TCP
// handshake. No Timeout — a streaming body is unbounded by design, the same
// reason cmd/ezllm/main.go sets no WriteTimeout.
func NewClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        512,
			MaxIdleConnsPerHost: 256,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
	}
}

// ── payloads (fixed bytes, built once) ───────────────────────────────────────

func filler(n int) []byte { return bytes.Repeat([]byte("x"), n) }

func completionBody(contentBytes int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":"mock-cmpl","object":"chat.completion","created":1,"model":"mock-model","choices":[{"index":0,"message":{"role":"assistant","content":"`)
	b.Write(filler(contentBytes))
	b.WriteString(`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
	return b.Bytes()
}

func chunkFrame(i, contentBytes int) []byte {
	var b bytes.Buffer
	b.WriteString(`data: {"id":"mock-cmpl","object":"chat.completion.chunk","created":1,"model":"mock-model","choices":[{"index":`)
	b.WriteString(strconv.Itoa(i))
	b.WriteString(`,"delta":{"content":"`)
	b.Write(filler(contentBytes))
	b.WriteString(`"},"finish_reason":null}]}`)
	b.WriteString("\n\n")
	return b.Bytes()
}

// terminalFrame carries the usage the ledger taps. It is byte-for-byte the
// shape internal/proxy.tapSSELine expects on the OpenAI surface.
func terminalFrame() []byte {
	return []byte(`data: {"id":"mock-cmpl","object":"chat.completion.chunk","created":1,"model":"mock-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\n")
}

// ChatBody builds a deterministic OpenAI chat request body of roughly
// totalBytes, carrying the given model. The padding lives in a user message so
// the body stays valid JSON at every size.
func ChatBody(model string, totalBytes int) []byte {
	const envelope = `{"model":"","messages":[{"role":"user","content":""}],"temperature":0.7}`
	pad := totalBytes - len(envelope) - len(model)
	if pad < 0 {
		pad = 0
	}
	var b bytes.Buffer
	b.WriteString(`{"model":`)
	enc, _ := json.Marshal(model)
	b.Write(enc)
	b.WriteString(`,"messages":[{"role":"user","content":"`)
	b.Write(filler(pad))
	b.WriteString(`"}],"temperature":0.7}`)
	return b.Bytes()
}

// StreamChatBody is ChatBody plus `"stream":true`.
func StreamChatBody(model string, totalBytes int) []byte {
	body := ChatBody(model, totalBytes)
	return []byte(strings.TrimSuffix(string(body), "}") + `,"stream":true}`)
}
