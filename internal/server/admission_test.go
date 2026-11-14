package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type admissionReadSignal struct {
	io.ReadCloser
	once    sync.Once
	started chan int
}

func (r *admissionReadSignal) Read(p []byte) (int, error) {
	r.once.Do(func() { r.started <- len(p) })
	return r.ReadCloser.Read(p)
}

func TestSecurityRegressionAdmissionRejectsBeforeAllocatingBody(t *testing.T) {
	for _, tc := range []struct {
		name    string
		limits  RequestLimits
		tokens  []string
		reject  string
		chunked bool
	}{
		{"per client", RequestLimits{4, 2, 8}, []string{"tok-infer", "tok-infer"}, "tok-infer", false},
		{"global", RequestLimits{2, 2, 8}, []string{"tok-infer", "tok-admin"}, "tok-infer", false},
		{"memory", RequestLimits{4, 2, 2}, []string{"tok-infer"}, "tok-admin", false},
		{"chunked memory", RequestLimits{4, 2, 2}, []string{"tok-infer"}, "tok-admin", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			starts := make(chan int, 8)
			h := newHarness(t, func(http.ResponseWriter, *http.Request) { t.Error("incomplete body reached upstream") }, func(o *Options) { o.MaxBodyMiB = 2; o.RequestLimits = tc.limits })
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.Body = &admissionReadSignal{ReadCloser: r.Body, started: starts}
				h.handler.ServeHTTP(w, r)
			}))
			defer gateway.Close()
			var held []net.Conn
			defer func() {
				for _, c := range held {
					c.Close()
				}
			}()
			dial := func(token string, chunked bool) net.Conn {
				c, err := net.Dial("tcp", strings.TrimPrefix(gateway.URL, "http://"))
				if err != nil {
					t.Fatal(err)
				}
				headers := "Content-Length: 2097152\r\n"
				body := "{"
				if chunked {
					headers = "Transfer-Encoding: chunked\r\n"
					body = "1\r\n{\r\n"
				}
				fmt.Fprintf(c, "POST /v1/chat/completions HTTP/1.1\r\nHost: gateway\r\nAuthorization: Bearer %s\r\n%sContent-Type: application/json\r\n\r\n%s", token, headers, body)
				return c
			}
			for _, token := range tc.tokens {
				c := dial(token, tc.chunked)
				held = append(held, c)
				select {
				case <-starts:
				case <-time.After(2 * time.Second):
					t.Fatal("first requests did not enter body reader")
				}
			}
			rejected := dial(tc.reject, false)
			defer rejected.Close()
			rejected.SetReadDeadline(time.Now().Add(time.Second))
			res, err := http.ReadResponse(bufio.NewReader(rejected), nil)
			if err != nil {
				t.Fatalf("rejection drained stalled body: %v", err)
			}
			res.Body.Close()
			if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
				t.Fatalf("status %d", res.StatusCode)
			}
			select {
			case <-starts:
				t.Fatal("rejected request allocated/read a body")
			default:
			}
			h.server.admission.mu.Lock()
			used, active := h.server.admission.used, h.server.admission.active
			h.server.admission.mu.Unlock()
			if used != int64(len(tc.tokens))*2<<20 || active != len(tc.tokens) {
				t.Fatalf("used=%d active=%d", used, active)
			}
			for _, c := range held {
				c.Close()
			}
			deadline := time.Now().Add(time.Second)
			for {
				h.server.admission.mu.Lock()
				empty := h.server.admission.active == 0 && h.server.admission.used == 0 && len(h.server.admission.clients) == 0
				h.server.admission.mu.Unlock()
				if empty {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("cancelled body retained admission")
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

func TestSecurityRegressionAdmissionRetainsActualBodyUntilCompletion(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			okUpstream(w, r)
		case <-r.Context().Done():
		}
	}, func(o *Options) { o.MaxBodyMiB = 2; o.RequestLimits = RequestLimits{1, 1, 2} })
	gateway := httptest.NewServer(h.handler)
	defer gateway.Close()
	defer close(release)
	body := `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hello"}]}`
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest("POST", gateway.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok-infer")
		res, err := gateway.Client().Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach upstream")
	}
	h.server.admission.mu.Lock()
	used, active := h.server.admission.used, h.server.admission.active
	h.server.admission.mu.Unlock()
	if used != int64(len(body)) || active != 1 {
		t.Fatalf("body reservation lost while upstream active: %d/%d", used, active)
	}
	code, _, _ := h.do("POST", "/v1/chat/completions", "tok-admin", body)
	if code != 429 {
		t.Fatal(code)
	}
	// Let the real HTTP request finish before deferred server cleanup.
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish")
	}
	h.server.admission.mu.Lock()
	defer h.server.admission.mu.Unlock()
	if h.server.admission.active != 0 || h.server.admission.used != 0 || len(h.server.admission.clients) != 0 {
		t.Fatal("completed request leaked admission")
	}
}

func TestSecurityRegressionAdmissionReleasesOnPanicAndMalformedBody(t *testing.T) {
	h := newHarness(t, okUpstream, func(o *Options) { o.RequestLimits = RequestLimits{1, 1, 32} })
	body := `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`
	original := h.server.resolver
	h.server.resolver = nil
	if code, _, _ := h.do("POST", "/v1/chat/completions", "tok-infer", body); code != 500 {
		t.Fatal(code)
	}
	h.server.resolver = original
	if code, _, _ := h.do("POST", "/v1/chat/completions", "tok-infer", "{"); code != 400 {
		t.Fatal(code)
	}
	if code, _, _ := h.do("POST", "/v1/chat/completions", "tok-infer", body); code != 200 {
		t.Fatal(code)
	}
}
