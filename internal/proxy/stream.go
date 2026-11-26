package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// streamCopyTracked copies SSE upstream->client with a Flush per chunk, observing
// usage events without altering a single byte. Returns (ttft, usage, err) where
// err is non-nil when the stream was truncated — the caller records it, since
// the 200 status has already been sent and cannot be changed.
//
// `start` is the DISPATCH time (when the upstream request was posted), not the
// moment streaming began. TTFT means "time to first token": the provider's
// prefill happens before it emits any byte, so measuring from inside this
// function — after headers already arrived — reports a meaningless 1-3ms and
// hides exactly the cache/prefill latency the number exists to reveal.
//
// The third return is the first non-terminal SSE data payload. A streamed
// error response carries its reason in there, and the ledger's err column is
// blank for streams unless the caller is handed it.
//
// requireTerminal treats a stream that ends without its terminal event as
// truncated (native streams whose wire defines one).
func (d *Dispatcher) streamCopyTracked(ctx context.Context, w http.ResponseWriter, resp *http.Response, surface provider.Surface, conv provider.UsageConvention, start time.Time, requireTerminal bool) (time.Duration, *store.Usage, []byte, error) {
	ttft, tap, first, err := d.streamCopyTap(ctx, w, resp, surface, conv, start, requireTerminal)
	return ttft, tap.Usage(), first, err
}

var sseReaders = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 64*1024) }}

// streamCopyTap is streamCopyTracked returning the tapper itself, for callers
// that read more than usage from it (the Responses id).
func (d *Dispatcher) streamCopyTap(ctx context.Context, w http.ResponseWriter, resp *http.Response, surface provider.Surface, conv provider.UsageConvention, start time.Time, requireTerminal bool) (time.Duration, *UsageTapper, []byte, error) {
	var ttft time.Duration
	var firstEvent bool
	var firstPayload []byte

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	// Headers already copied; write status now so the client starts receiving.
	w.WriteHeader(resp.StatusCode)
	flush()

	// Lines are read with ReadSlice and written back VERBATIM, line ending
	// included (CRLF stays CRLF), so forwarding is byte-exact. A line longer
	// than the buffer arrives in pieces: every piece is forwarded at once,
	// and only lines that fit (maxScanToken) are tapped — the usage chunk
	// after a giant content line is still read.
	br := sseReaders.Get().(*bufio.Reader)
	br.Reset(resp.Body)
	defer func() {
		br.Reset(nil) // Do not retain the upstream response between streams.
		sseReaders.Put(br)
	}()
	// Read-only usage tap. Merges across events: Anthropic's message_start
	// carries the input/cache breakdown while message_delta carries the final
	// output totals, so taking only the first hit would record out=0.
	tapper := NewUsageTapperFor(surface, conv)
	var long []byte   // a line spanning several buffer fills, while it fits
	overlong := false // the current line outgrew maxScanToken: not tapped
	for {
		piece, rerr := br.ReadSlice('\n')
		if len(piece) > 0 {
			if _, err := w.Write(piece); err != nil {
				return ttft, tapper, firstPayload, fmt.Errorf("client write: %w", err)
			}
			complete := rerr == nil || rerr == io.EOF
			line := piece
			if !complete || long != nil || overlong {
				if !overlong {
					if len(long)+len(piece) > maxScanToken {
						long, overlong = nil, true
					} else {
						long = append(long, piece...)
					}
				}
				line = long
			}
			if complete {
				if !overlong {
					line = bytes.TrimRight(line, "\r\n")
					// TTFT is the first EVENT: blank separators and SSE
					// comments (": keepalive", sent by some gateways the
					// moment the request lands) are not the first token.
					if !firstEvent && len(line) > 0 && line[0] != ':' {
						ttft = time.Since(start)
						firstEvent = true
					}
					// Remember the first real payload so the caller can
					// quote it in the ledger. SSE metadata and [DONE]
					// cannot carry an error envelope; a bare line can, from
					// a gateway that labels a plain error body as SSE.
					if firstPayload == nil && !isSSEMetadata(line) {
						if p := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))); len(p) > 0 && !bytes.Equal(p, []byte("[DONE]")) {
							firstPayload = append([]byte(nil), p...)
						}
					}
					tapper.Observe(line)
				} else if !firstEvent {
					tapper.event, tapper.skipEvent = nil, true
					ttft = time.Since(start)
					firstEvent = true
				}
				if overlong {
					tapper.event, tapper.skipEvent = nil, true
				}
				long, overlong = nil, false
			}
			// Flush only when nothing more is already buffered: the client
			// sees every byte as soon as the upstream stops sending, and a
			// burst of events costs one flush instead of one per line.
			if br.Buffered() == 0 {
				flush()
			}
		}
		if rerr == nil || rerr == bufio.ErrBufferFull {
			continue
		}
		flush()
		if !firstEvent {
			ttft = time.Since(start)
		}
		if rerr == io.EOF {
			if requireTerminal && !tapper.terminal {
				return ttft, tapper, firstPayload, errors.New("upstream stream ended without a terminal event")
			}
			return ttft, tapper, firstPayload, nil
		}
		// The upstream died mid-stream. The 200 is already committed, so the
		// only honest signal left is a terminal SSE error frame in the
		// client's dialect — otherwise the client is left with a silent EOF.
		writeStreamErrorFrame(w, surface, "upstream connection lost")
		flush()
		return ttft, tapper, firstPayload, fmt.Errorf("upstream read: %w", rerr)
	}
}

// writeStreamErrorFrame emits a terminal SSE error event in the client's
// surface dialect. Called only after a committed 200 when the upstream dies
// mid-stream, so the client gets an explicit error instead of a silent EOF.
func writeStreamErrorFrame(w io.Writer, surface provider.Surface, msg string) {
	m, _ := json.Marshal(msg)
	// The upstream can die mid-line. A blank line first ends whatever partial
	// event was written, so the error frame is parsed on its own; between
	// complete events it is an empty line, which SSE ignores.
	io.WriteString(w, "\n\n")
	switch surface {
	case provider.SurfaceAnthropic:
		fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":%s}}\n\n", m)
	case provider.SurfaceResponses:
		fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":%s}\n\n", m)
	default: // openai
		fmt.Fprintf(w, "data: {\"error\":{\"type\":\"server_error\",\"message\":%s}}\n\n", m)
	}
}

// sseUsageRaw returns the raw usage object one SSE data line carries, or nil.
// Where usage rides is per surface: openai puts it on the final chunk;
// anthropic sends it on message_start AND message_delta (the delta carries the
// authoritative output_tokens, the start the input/cache breakdown, so the tap
// merges across events); responses puts the full usage on response.completed.
// Every usage-bearing event names "usage", so a line without the word is
// skipped before any JSON decoding — content deltas are most of a stream.
func sseUsageRaw(line []byte, surface provider.Surface) map[string]any {
	payload := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
	if len(payload) == 0 || !bytes.Contains(payload, []byte(`"usage"`)) {
		return nil
	}
	var ev map[string]any
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil
	}
	switch surface {
	case provider.SurfaceAnthropic:
		typ, _ := ev["type"].(string)
		switch typ {
		case "message_start":
			if msg, ok := ev["message"].(map[string]any); ok {
				if u, ok := msg["usage"].(map[string]any); ok {
					return u
				}
			}
		case "message_delta":
			if u, ok := ev["usage"].(map[string]any); ok {
				return u
			}
		}
	case provider.SurfaceResponses:
		typ, _ := ev["type"].(string)
		// Usage rides on the TERMINAL event — which is response.completed on
		// success but response.incomplete when max_output_tokens truncated the
		// response (observed live). Also accept response.failed rather than
		// silently recording zero usage for a truncated stream.
		switch typ {
		case "response.completed", "response.incomplete", "response.failed":
			if r, ok := ev["response"].(map[string]any); ok {
				if u, ok := r["usage"].(map[string]any); ok {
					return u
				}
			}
		}
		if u, ok := ev["usage"].(map[string]any); ok {
			return u
		}
	default: // openai
		if u, ok := ev["usage"].(map[string]any); ok {
			return u
		}
	}
	return nil
}

// responsesBodyID reads a complete Responses body's id ("" when absent).
func responsesBodyID(raw []byte) string {
	var body struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	return body.ID
}

// tapJSONUsage extracts usage from a complete (non-streamed) JSON body.
func tapJSONUsage(body []byte, surface provider.Surface, conv provider.UsageConvention) *store.Usage {
	var ev map[string]any
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil
	}
	if u, ok := ev["usage"].(map[string]any); ok {
		n := store.NormalizeUsageWith(usageSurface(surface), u, conv)
		return &n
	}
	return nil
}

// tapConvention is how the tap reads a Messages-shape usage object for this
// hop: the account's declared convention for its own native bytes; exclusive
// for a translated body, whose usage the codec already normalized.
func tapConvention(rt Route, cross bool) provider.UsageConvention {
	if cross {
		return provider.UsageConventionAuto
	}
	return rt.Account.UsageConvention
}

// --- header plumbing ---
