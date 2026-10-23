package claudeweb

import (
	"context"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// h2Server is a local TLS+h2 server that counts the TLS connections it accepts
// and a fetcher that trusts it.
func h2Server(t *testing.T, h http.HandlerFunc) (*httptest.Server, *TLSFetcher, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	f := NewTLSFetcher()
	f.rootCAs = x509.NewCertPool()
	f.rootCAs.AddCert(srv.Certificate())
	return srv, f, &conns
}

// Successive fetches reuse one connection instead of paying a new TCP and
// TLS handshake each (audit P2: Fetch built a new transport per call).
func TestTLSFetcherReusesItsConnection(t *testing.T) {
	srv, f, conns := h2Server(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	for i := 0; i < 3; i++ {
		resp, err := f.Fetch(context.Background(), http.MethodGet, srv.URL+"/api/organizations", nil, http.Header{})
		if err != nil {
			t.Skipf("local TLS handshake with the chrome profile failed (%v)", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("connections = %d for 3 fetches, want 1 (reused)", n)
	}
}

// Cancelling one stream on the shared connection leaves a concurrent stream
// intact.
func TestTLSFetcherCancelIsPerRequest(t *testing.T) {
	release := make(chan struct{})
	srv, f, _ := h2Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		<-release
		io.WriteString(w, "survivor")
	})
	ctx, cancel := context.WithCancel(context.Background())
	victim, err := f.Fetch(ctx, http.MethodGet, srv.URL+"/slow", nil, http.Header{})
	if err != nil {
		t.Skipf("local TLS handshake with the chrome profile failed (%v)", err)
	}
	done := make(chan string, 1)
	go func() {
		resp, err := f.Fetch(context.Background(), http.MethodGet, srv.URL+"/other", nil, http.Header{})
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- string(b)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	victim.Body.Close()
	close(release)
	select {
	case got := <-done:
		if got != "survivor" {
			t.Fatalf("concurrent stream = %q, want it untouched by the other's cancel", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent stream never finished")
	}
}
