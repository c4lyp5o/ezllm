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

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// apiBase is the claude.ai API root.
const apiBase = "https://claude.ai/api"

// chromeMajor is the TLS-fingerprint profile (omniroute: chrome_146). bump
// when Cloudflare starts rejecting the profile.
const chromeMajor = 146

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

// TLSFetcher is the production fetcher: uTLS Chrome ClientHello + h2.
type TLSFetcher struct {
	Timeout       time.Duration // total request timeout; 0 = none (ctx governs)
	chromeHelloID utls.ClientHelloID
}

// NewTLSFetcher builds the browser-impersonating fetcher.
func NewTLSFetcher() *TLSFetcher {
	return &TLSFetcher{Timeout: 60 * time.Second, chromeHelloID: utls.HelloChrome_Auto}
}

// Fetch dials with the Chrome ClientHello, negotiates h2, and performs the
// request. A challenge/error body is returned to the caller like omniroute's
// transport does — classification happens above (isClaudeWebChallenge).
func (f *TLSFetcher) Fetch(ctx context.Context, method, url string, body []byte, headers http.Header) (*Response, error) {
	if f.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.Timeout)
		defer cancel()
	}
	tr := &http2.Transport{
		AllowHTTP: false,
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
				// claude.ai sits behind Cloudflare; the browser does not pin
				// the leaf either for this profile — validation is not the
				// fingerprint's job.
				InsecureSkipVerify: true,
			}, f.chromeHelloID)
			if err := uconn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, fmt.Errorf("claudeweb: tls handshake: %w", err)
			}
			return uconn, nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, method, url, byteReader(body))
	if err != nil {
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
		return nil, err
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

// plainFetcher is the test fetcher: plain HTTP against a local mock. It
// intentionally carries NO browser fingerprint.
type plainFetcher struct {
	base string // e.g. http://127.0.0.1:PORT
}

func (p plainFetcher) Fetch(ctx context.Context, method, url string, body []byte, headers http.Header) (*Response, error) {
	if strings.HasPrefix(url, apiBase) {
		url = p.base + strings.TrimPrefix(url, apiBase)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, byteReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
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

var errUnauthorized = errors.New("claudeweb: session expired or invalid")
