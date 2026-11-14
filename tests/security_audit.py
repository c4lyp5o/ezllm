#!/usr/bin/env python3
"""Bounded, synthetic reproductions for the 2026-11-14 security audit.

An exit status of zero means the documented findings reproduced, NOT that the
application is secure. These are audit evidence, not acceptance/regression tests.
No live service, credential, or model is used. Payload-buffer allocation is
limited to 16 MiB. Run from any directory with Python 3 and the project Go toolchain.
Temporary in-package Go probes are created exclusively and removed on exit.
"""

from pathlib import Path
import subprocess
import sys

SERVER_PROBE = r'''
package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// Audit reproductions intentionally assert current vulnerable behavior. They
// are temporary evidence, not regression tests endorsing that behavior.
func TestSecurityAuditDisabledVSCodeCredentialLogged(t *testing.T) {
	h, sink := harnessWithLog(t, slog.LevelDebug, okUpstream)
	const credential = "synthetic-audit-disabled-credential"
	id, _, err := h.db.UpsertToken(context.Background(), store.TokenInput{Name: "audit-key", Roles: []string{"infer"}, Token: credential})
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	if err = h.db.UpdateToken(context.Background(), id, &disabled, nil, nil, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	code, _, _ := h.do("GET", "/api/v1/vscode/"+credential+"/models", "", "")
	if code != 401 {
		t.Fatal(code)
	}
	leaked := strings.Contains(sink.String(), credential)
	if !leaked {
		t.Fatal("hypothesis disproved: no raw credential in debug log")
	}
	// Demonstrate this disabled credential is reusable after re-enabling it.
	enabled := true
	if err = h.db.UpdateToken(context.Background(), id, &enabled, nil, nil, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	code, _, _ = h.do("GET", "/v1/models", credential, "")
	if code != 200 {
		t.Fatal(code)
	}
	t.Log("confirmed: disabled path credential logged in full; same credential authenticates after re-enable")
}

func TestSecurityAuditUpstreamCanSetAdminCookie(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "ezllm_session=synthetic-provider-controlled; Path=/admin; HttpOnly; SameSite=Strict")
		okUpstream(w, r)
	})
	gateway := httptest.NewServer(h.handler)
	defer gateway.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	req, _ := http.NewRequest("POST", gateway.URL+"/v1/chat/completions", strings.NewReader(`{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	target, _ := url.Parse(gateway.URL + "/admin/accounts")
	found := false
	for _, c := range jar.Cookies(target) {
		if c.Name == "ezllm_session" && c.Value == "synthetic-provider-controlled" {
			found = true
		}
	}
	if !found {
		t.Fatal("hypothesis disproved: upstream cookie not accepted for gateway admin path")
	}
	t.Log("confirmed: provider response replaces gateway-origin admin session cookie in cookie jar; no account takeover claimed")
}

type auditReadSignal struct {
	io.ReadCloser
	once    sync.Once
	started chan int
}

func (r *auditReadSignal) Read(p []byte) (int, error) {
	r.once.Do(func() { r.started <- len(p) })
	return r.ReadCloser.Read(p)
}
func TestSecurityAuditConcurrentBodyAllocationsBeforeAdmission(t *testing.T) {
	const requests = 8
	const size = 2 << 20
	starts := make(chan int, requests)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { t.Error("upstream should not be called") }, func(o *Options) { o.MaxBodyMiB = 2 })
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &auditReadSignal{ReadCloser: r.Body, started: starts}
		h.handler.ServeHTTP(w, r)
	}))
	defer gateway.Close()
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	total := 0
	for i := 0; i < requests; i++ {
		c, err := net.Dial("tcp", strings.TrimPrefix(gateway.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		fmt.Fprintf(c, "POST /v1/chat/completions HTTP/1.1\r\nHost: gateway\r\nAuthorization: Bearer tok-infer\r\nContent-Length: %d\r\nContent-Type: application/json\r\n\r\n{", size)
		select {
		case n := <-starts:
			total += n
		case <-time.After(2 * time.Second):
			t.Fatal("request not admitted into body read")
		}
	}
	if total != requests*size {
		t.Fatalf("allocated read slices %d", total)
	}
	t.Logf("confirmed: %d authenticated connections sending one byte each hold %d bytes of payload buffers before routing/upstream admission", requests, total)
}

func TestSecurityAuditSettingsBodyHasNoReadBudget(t *testing.T) {
	old := adminBodyTimeout
	adminBodyTimeout = 100 * time.Millisecond
	defer func() { adminBodyTimeout = old }()
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	gateway := httptest.NewServer(h.handler)
	defer gateway.Close()
	c, err := net.Dial("tcp", strings.TrimPrefix(gateway.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "PATCH /admin/settings HTTP/1.1\r\nHost: gateway\r\nAuthorization: Bearer tok-admin\r\nContent-Length: 100\r\nContent-Type: application/json\r\n\r\n{\"revision\"")
	c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err == nil {
		res.Body.Close()
		t.Fatalf("hypothesis disproved: response %d under read budget", res.StatusCode)
	}
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatal(err)
	}
	t.Log("confirmed: settings body remains pending beyond 5x the configured admin body-read budget")
}
'''

CODEX_PROBE = r'''
package codexbridge

import (
	"context"
	"strings"
	"testing"
)

func TestSecurityAuditKnownToolIDResumesUnrelatedHistory(t *testing.T) {
	r, home := fakeRunner(t, "tools")
	r.ForkCache = true
	defer r.Shutdown()
	first := weatherRequest()
	first.Input = []UserInput{{Type: "text", Text: "Caller A private conversation; request the weather tool."}}
	result, err := r.Run(context.Background(), first, nil)
	if err != nil || len(result.ToolCalls) != 1 {
		t.Fatalf("setup failed: %v", err)
	}
	// Knowledge of the pending ID is a prerequisite, not an ID-guessing proof.
	unrelated := weatherRequest()
	unrelated.Input = []UserInput{{Type: "text", Text: "Caller B unrelated history; original context is absent."}}
	unrelated.ToolResults = map[string]string{result.ToolCalls[0].ID: "synthetic-injected-result"}
	final, err := r.Run(context.Background(), unrelated, nil)
	if err != nil || !strings.Contains(final.Text, "synthetic-injected-result") || spawnCount(t, home) != 1 {
		t.Fatalf("hypothesis disproved: err=%v spawns=%d", err, spawnCount(t, home))
	}
	t.Log("confirmed: known tool-call ID plus matching configuration resumes prior child despite unrelated caller input; no authenticated caller identity exists in Request")
}
'''

def main():
    root = Path(__file__).resolve().parents[1]
    created = []
    try:
        for package, source in (("server", SERVER_PROBE), ("codexbridge", CODEX_PROBE)):
            path = root / "internal" / package / "security_audit_probe_test.go"
            # Never overwrite an operator's or another audit's test file.
            with path.open("x", encoding="utf-8") as handle:
                created.append(path)
                handle.write(source.lstrip("\n"))
        print("Reproducing documented findings with synthetic localhost fixtures.", flush=True)
        print("PASS means the finding reproduced; it is not a security pass.", flush=True)
        result = subprocess.run(
            ["go", "test", "-short", "./internal/server", "./internal/codexbridge",
             "-run", "^TestSecurityAudit", "-count=1", "-v"], cwd=root, timeout=120)
        return result.returncode
    finally:
        for path in reversed(created):
            path.unlink()


if __name__ == "__main__":
    sys.exit(main())
