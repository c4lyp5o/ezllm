// Package claudeweb implements Claude.ai's WEB protocol for ezllm, ported
// from omniroute's production implementation (claude-web.ts +
// claudeTlsClient.ts, extracted from the image 2026-10-16; design:
// docs/claude-web-design.md, cross-ref: the stale community digest
// ~/workspace/claude-ai-re-client-protocol-digest.md).
//
// M14 scope: sessionKey cookie auth, organization discovery, one
// conversation per router session, SSE completion parsing into deltas.
// No image input, no artifacts, no model discovery (ids pass through).
package claudeweb

//lint:file-ignore SA1019 x/net/http2.Transport is required here: the
// connection is a uTLS Chrome-fingerprinted conn, not a *tls.Conn, and
// net/http's Transport only negotiates HTTP/2 over *tls.Conn. The
// "use http.Transport" replacement cannot speak h2 on this connection.

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// apiBase is the claude.ai API root.
const apiBase = "https://claude.ai/api"

// Response is one completed upstream read.
type Response struct {
	Status int
	Header http.Header
	Body   io.ReadCloser // streaming; caller closes
}

// Fetcher performs one HTTPS request with the browser TLS profile. The
// interface exists so tests inject a plain-HTTP fetcher (the fingerprint
// machinery is exercised once, against a local TLS server).
type Fetcher interface {
	Fetch(ctx context.Context, method, url string, body []byte, headers http.Header) (*Response, error)
}

// TLSFetcher is the production fetcher: uTLS Chrome ClientHello + h2. One
// transport serves every Fetch, so successive organization and completion
// requests share a connection instead of each paying a TCP and TLS
// handshake. h2 multiplexes concurrent requests as streams: cancelling one
// resets only its stream. Credentials ride each request's headers, never the
// transport, so a shared connection carries no session state of its own.
type TLSFetcher struct {
	Timeout       time.Duration // total request timeout; 0 = none (ctx governs)
	chromeHelloID utls.ClientHelloID
	rootCAs       *x509.CertPool // nil = system roots; tests pin a local CA

	once sync.Once
	tr   *http2.Transport
}

// fetcherIdleTimeout closes a connection nothing has used for a while — in
// particular the one held by a fetcher whose session was replaced.
const fetcherIdleTimeout = 90 * time.Second

// Close drops the fetcher's idle connections.
func (f *TLSFetcher) Close() {
	// Share transport's once barrier: a cookie replacement may close this
	// fetcher while its first request is still initializing the transport.
	f.transport().CloseIdleConnections()
}

func (f *TLSFetcher) transport() *http2.Transport {
	f.once.Do(func() {
		f.tr = &http2.Transport{
			AllowHTTP:       false,
			IdleConnTimeout: fetcherIdleTimeout,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					host = addr
				}
				raw, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				// HelloChrome_Auto carries Chrome's ClientHello AND its ALPN
				// (h2, http/1.1); http2.Transport keys off the negotiated proto.
				uconn := utls.UClient(raw, &utls.Config{
					ServerName: host,
					MinVersion: tls.VersionTLS13,
					// Chain and hostname verification stay on: the session cookie
					// and every prompt ride this connection. The fingerprint only
					// shapes the ClientHello; it never needs verification off.
					RootCAs: f.rootCAs,
				}, f.chromeHelloID)
				if err := uconn.HandshakeContext(ctx); err != nil {
					raw.Close()
					return nil, fmt.Errorf("claudeweb: tls handshake: %w", err)
				}
				return uconn, nil
			},
		}
	})
	return f.tr
}

// NewTLSFetcher builds the browser-impersonating fetcher.
func NewTLSFetcher() *TLSFetcher {
	return &TLSFetcher{Timeout: 60 * time.Second, chromeHelloID: utls.HelloChrome_Auto}
}

// Fetch dials with the Chrome ClientHello, negotiates h2, and performs the
// request. A challenge/error body is returned to the caller like omniroute's
// transport does — classification happens above (isClaudeWebChallenge).
func (f *TLSFetcher) Fetch(ctx context.Context, method, url string, body []byte, headers http.Header) (*Response, error) {
	// The timeout must outlive this call: the caller reads the returned body
	// (SSE completions stream for many seconds). Cancelling on return kills
	// every read with "context canceled" — so tie the cancel to Body.Close
	// instead of deferring it here.
	var cancel context.CancelFunc
	if f.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, f.Timeout)
	} else {
		cancel = func() {}
	}
	tr := f.transport()
	req, err := http.NewRequestWithContext(ctx, method, url, byteReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// http2 requires URL.Scheme https and a Host header.
	req.URL.Scheme = "https"
	if req.Host == "" {
		req.Host = req.URL.Host
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		cancel()
		return nil, err
	}
	decoded, err := decodeBody(resp)
	if err != nil {
		resp.Body.Close()
		cancel()
		return nil, err
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: &cancelOnClose{ReadCloser: decoded, cancel: cancel}}, nil
}

// cancelOnClose releases the per-request timeout when the caller closes the
// body — the streaming callers read it to EOF first.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// decodeBody decompresses the response according to Content-Encoding. The
// browser header advertises gzip/br/zstd, so Cloudflare answers compressed;
// without this the JSON body is unreadable binary (the mock tests never
// compress, so this only bites live).
func decodeBody(resp *http.Response) (io.ReadCloser, error) {
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return resp.Body, nil
	case "gzip":
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("claudeweb: gzip: %w", err)
		}
		return struct {
			io.Reader
			io.Closer
		}{zr, resp.Body}, nil
	case "br":
		return struct {
			io.Reader
			io.Closer
		}{brotli.NewReader(resp.Body), resp.Body}, nil
	case "zstd":
		zr, err := zstd.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("claudeweb: zstd: %w", err)
		}
		return struct {
			io.Reader
			io.Closer
		}{zr, resp.Body}, nil
	default:
		return resp.Body, nil
	}
}

func byteReader(b []byte) io.Reader {
	if b == nil {
		return http.NoBody
	}
	return strings.NewReader(string(b))
}

// BrowserHeaders is the exact header set omniroute's claude-web sends
// (getBrowserHeaders + CLAUDE_WEB_FINGERPRINT). Device id rides along when
// known.
func BrowserHeaders(deviceID, locale, referer string) http.Header {
	h := http.Header{}
	h.Set("Accept", "text/event-stream")
	h.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	h.Set("Accept-Language", firstNon(locale, "en-US")+",en;q=0.9")
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Type", "application/json")
	h.Set("Origin", "https://claude.ai")
	h.Set("Pragma", "no-cache")
	h.Set("Priority", "u=1, i")
	h.Set("Referer", firstNon(referer, "https://claude.ai/new"))
	h.Set("Sec-Ch-Ua", `"Chromium";v="146", "Google Chrome";v="146", "Not_A Brand";v="24"`)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Platform", `"Linux"`)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")
	h.Set("anthropic-client-platform", "web_claude_ai")
	if deviceID != "" {
		h.Set("anthropic-device-id", deviceID)
	}
	return h
}

func firstNon(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
