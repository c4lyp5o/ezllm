package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const tgToken = "123456789:AAH-redacted-token-value-for-tests-0123"

var msg = Message{Kind: "bridge_rolled_back", Severity: "error", Title: "Claude Pro/Max CLI 2.1.312 rolled back",
	Body: "The release failed after promotion; the bridge is back on 2.1.308.", CreatedAt: "2026-10-31T03:12:40Z"}

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		name, kind      string
		config, secrets map[string]string
		says            string
	}{
		{"ntfy ok", "ntfy", map[string]string{"topic": "ezllm-alerts"}, nil, ""},
		{"ntfy self-hosted", "ntfy", map[string]string{"server": "https://ntfy.example.com", "topic": "a"}, map[string]string{"token": "tk_x"}, ""},
		{"ntfy topic shape", "ntfy", map[string]string{"topic": "has space"}, nil, "topic must be"},
		{"ntfy server with credentials", "ntfy", map[string]string{"server": "https://u:p@ntfy.sh", "topic": "a"}, nil, "server must not carry credentials"},
		{"ntfy server scheme", "ntfy", map[string]string{"server": "file:///etc/passwd", "topic": "a"}, nil, "server must be an absolute http"},
		{"discord ok", "discord", nil, map[string]string{"webhook_url": "https://discord.com/api/webhooks/1/abc"}, ""},
		{"discord elsewhere", "discord", nil, map[string]string{"webhook_url": "https://evil.example/api/webhooks/1"}, "Discord webhook URL"},
		{"telegram ok", "telegram", map[string]string{"chat_id": "-1001234567890"}, map[string]string{"bot_token": tgToken}, ""},
		{"telegram channel name", "telegram", map[string]string{"chat_id": "@ezllm_ops"}, map[string]string{"bot_token": tgToken}, ""},
		{"telegram bad token", "telegram", map[string]string{"chat_id": "42"}, map[string]string{"bot_token": "nope"}, "bot_token must be"},
		{"webhook ok", "webhook", nil, map[string]string{"url": "https://hooks.example.com/x?key=s3cret"}, ""},
		{"webhook relative", "webhook", nil, map[string]string{"url": "/hook"}, "url must be an absolute"},
		{"missing required", "telegram", map[string]string{"chat_id": "42"}, nil, "telegram needs bot_token"},
		{"secret given as config", "discord", map[string]string{"webhook_url": "https://discord.com/api/webhooks/1/a"}, nil, `does not take config field "webhook_url"`},
		{"unknown field", "ntfy", map[string]string{"topic": "a", "colour": "red"}, nil, `does not take config field "colour"`},
		{"unknown kind", "slack", nil, nil, "kind must be one of"},
	} {
		err := Validate(c.kind, c.config, c.secrets)
		if (err == nil) != (c.says == "") || (err != nil && !strings.Contains(err.Error(), c.says)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.says)
		}
		for _, v := range c.secrets {
			if err != nil && len(v) > 4 && strings.Contains(err.Error(), v) {
				t.Errorf("%s: the error echoes a secret: %v", c.name, err)
			}
		}
	}
}

func decode(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body %s: %v", b, err)
	}
	return m
}

func TestBuildShapesEachKind(t *testing.T) {
	ctx := context.Background()
	r, _ := Build(ctx, "ntfy", map[string]string{"topic": "ops"}, map[string]string{"token": "tk_1"}, msg)
	b := decode(t, r)
	if r.URL.String() != DefaultNtfyServer || r.Header.Get("Authorization") != "Bearer tk_1" ||
		b["topic"] != "ops" || b["title"] != msg.Title || b["priority"] != float64(5) {
		t.Errorf("ntfy: %s %v %v", r.URL, r.Header, b)
	}

	r, _ = Build(ctx, "discord", nil, map[string]string{"webhook_url": "https://discord.com/api/webhooks/1/a"}, msg)
	b = decode(t, r)
	if !strings.HasPrefix(b["content"].(string), "**"+msg.Title+"**\n") || len(b["allowed_mentions"].(map[string]any)["parse"].([]any)) != 0 {
		t.Errorf("discord: %v", b)
	}

	r, _ = Build(ctx, "telegram", map[string]string{"chat_id": "42"}, map[string]string{"bot_token": tgToken}, msg)
	b = decode(t, r)
	if r.URL.Path != "/bot"+tgToken+"/sendMessage" || b["chat_id"] != "42" || b["parse_mode"] != nil || !strings.Contains(b["text"].(string), msg.Body) {
		t.Errorf("telegram: %s %v", r.URL.Path, b)
	}

	r, _ = Build(ctx, "webhook", nil, map[string]string{"url": "https://hooks.example.com/x", "bearer": "b3"}, msg)
	b = decode(t, r)
	if r.Header.Get("Authorization") != "Bearer b3" || b["kind"] != msg.Kind || b["severity"] != "error" || b["created_at"] != msg.CreatedAt {
		t.Errorf("webhook: %v %v", r.Header, b)
	}

	long := msg
	long.Body = strings.Repeat("x", 5000)
	r, _ = Build(ctx, "discord", nil, map[string]string{"webhook_url": "https://discord.com/api/webhooks/1/a"}, long)
	if n := len([]rune(decode(t, r)["content"].(string))); n != 2000 {
		t.Errorf("discord content %d runes, want clipped to 2000", n)
	}
}

func fastBackoff(t *testing.T) {
	old := Backoff
	Backoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { Backoff = old })
}

func TestSendRetriesOnlyWhatCanRecover(t *testing.T) {
	fastBackoff(t)
	for _, c := range []struct {
		name     string
		statuses []int
		calls    int32
		says     string
	}{
		{"ok first time", []int{204}, 1, ""},
		{"5xx then ok", []int{502, 200}, 2, ""},
		{"429 every time", []int{429, 429, 429}, 3, "answered 429"},
		{"4xx is final", []int{401}, 1, "refused the message (401)"},
		{"redirect is not followed", []int{302}, 1, "redirect (302)"},
	} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			if c.statuses[n-1] == 302 {
				w.Header().Set("Location", "https://elsewhere.example/")
			}
			w.WriteHeader(c.statuses[n-1])
		}))
		err := Send(context.Background(), Client(), "webhook", nil, map[string]string{"url": srv.URL, "bearer": "b3"}, msg, Attempts)
		srv.Close()
		if (err == nil) != (c.says == "") || (err != nil && !strings.Contains(err.Error(), c.says)) || calls.Load() != c.calls {
			t.Errorf("%s: err = %v after %d calls, want %q after %d", c.name, err, calls.Load(), c.says, c.calls)
		}
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: connect: connection refused (" + r.URL.String() + ")")
}

// A Telegram token is in the request path: no failure text may carry it.
func TestSendNeverReportsASecret(t *testing.T) {
	fastBackoff(t)
	client := &http.Client{Transport: failingTransport{}}
	err := Send(context.Background(), client, "telegram", map[string]string{"chat_id": "42"}, map[string]string{"bot_token": tgToken}, msg, 2)
	if err == nil || strings.Contains(err.Error(), tgToken) || strings.Contains(err.Error(), "AAH-redacted") {
		t.Fatalf("err = %v, want a failure without the token", err)
	}
	if !strings.Contains(err.Error(), "could not reach the channel") {
		t.Fatalf("err = %v, want it to say what failed", err)
	}
	// Redact matches a secret's exact text; a URL error can carry it escaped,
	// so the request URL is dropped before any text is kept.
	ue := &url.Error{Op: "Post", URL: "https://hooks.example.com/in?key=a%2Fb%2Fs3cret", Err: errors.New("dial tcp: connection refused")}
	if got := transportError(ue).Error(); strings.Contains(got, "hooks.example.com/in") || strings.Contains(got, "s3cret") || !strings.Contains(got, "connection refused") {
		t.Fatalf("transportError = %q, want the failure without the URL", got)
	}
	if got := Redact(errors.New("x "+tgToken+" y"), map[string]string{"bot_token": tgToken}).Error(); got != "x [redacted] y" {
		t.Fatalf("Redact = %q", got)
	}
}

func TestSeverityAndTarget(t *testing.T) {
	if !Meets("error", "warn") || !Meets("warn", "warn") || Meets("info", "warn") || ValidSeverity("fatal") {
		t.Fatal("severity order is info < warn < error")
	}
	for _, c := range [][4]string{
		{"ntfy", "", "ops", "https://ntfy.sh/ops"},
		{"telegram", "", "", "Telegram chat 42"},
		{"webhook", "", "", "https://hooks.example.com"},
	} {
		cfg := map[string]string{"topic": c[2], "chat_id": "42"}
		got := Target(c[0], cfg, map[string]string{"url": "https://hooks.example.com/x?key=s3cret"})
		if got != c[3] {
			t.Errorf("Target(%s) = %q, want %q", c[0], got, c[3])
		}
	}
}
