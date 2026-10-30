// Package notify delivers dashboard bell events to external push channels:
// ntfy, Discord, Telegram and a generic JSON webhook. It is pure — no store,
// no server: the server's push worker hands it a channel's fields and a
// message, and it builds, sends and judges the request.
//
// Secrets (ntfy token, Discord webhook URL, Telegram bot token, webhook URL
// and bearer) are inputs only: no request error, verdict or log line this
// package returns carries one (Redact).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Message is one bell event as a channel shows it.
type Message struct {
	Kind      string `json:"kind"`
	Severity  string `json:"severity"` // info | warn | error
	Title     string `json:"title"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// Field is one input a channel kind takes.
type Field struct {
	Name     string `json:"name"`
	Secret   bool   `json:"secret"`
	Required bool   `json:"required"`
}

// Kinds are the channel types, with their fields: config fields are stored
// and returned; secret fields are stored encrypted and never returned.
var Kinds = map[string][]Field{
	"ntfy":     {{Name: "server"}, {Name: "topic", Required: true}, {Name: "token", Secret: true}},
	"discord":  {{Name: "webhook_url", Secret: true, Required: true}},
	"telegram": {{Name: "chat_id", Required: true}, {Name: "bot_token", Secret: true, Required: true}},
	"webhook":  {{Name: "url", Secret: true, Required: true}, {Name: "bearer", Secret: true}},
}

// DefaultNtfyServer is used when an ntfy channel names no server.
const DefaultNtfyServer = "https://ntfy.sh"

var (
	ntfyTopic     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	telegramChat  = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z0-9_]{5,32})$`)
	telegramToken = regexp.MustCompile(`^[0-9]{5,15}:[A-Za-z0-9_-]{30,64}$`)
)

var severityRank = map[string]int{"info": 0, "warn": 1, "error": 2}

// ValidSeverity reports whether s is a bell severity.
func ValidSeverity(s string) bool { _, ok := severityRank[s]; return ok }

// Meets reports whether a message of severity reaches a channel whose
// minimum is min.
func Meets(severity, min string) bool { return severityRank[severity] >= severityRank[min] }

// Validate checks a channel's fields for its kind: no unknown field, every
// required one present, and each value in its shape. The error names the
// field, never its value.
func Validate(kind string, config, secrets map[string]string) error {
	fields, ok := Kinds[kind]
	if !ok {
		return fmt.Errorf("kind must be one of ntfy, discord, telegram, webhook")
	}
	known := map[string]Field{}
	for _, f := range fields {
		known[f.Name] = f
	}
	for name := range config {
		if f, ok := known[name]; !ok || f.Secret {
			return fmt.Errorf("%s does not take config field %q", kind, name)
		}
	}
	for name := range secrets {
		if f, ok := known[name]; !ok || !f.Secret {
			return fmt.Errorf("%s does not take secret field %q", kind, name)
		}
	}
	value := func(f Field) string {
		if f.Secret {
			return strings.TrimSpace(secrets[f.Name])
		}
		return strings.TrimSpace(config[f.Name])
	}
	for _, f := range fields {
		if f.Required && value(f) == "" {
			return fmt.Errorf("%s needs %s", kind, f.Name)
		}
	}
	switch kind {
	case "ntfy":
		if s := value(known["server"]); s != "" {
			if err := checkURL(s, false); err != nil {
				return fmt.Errorf("server %w", err)
			}
		}
		if !ntfyTopic.MatchString(value(known["topic"])) {
			return errors.New("topic must be 1 to 64 letters, digits, - or _")
		}
	case "discord":
		u := value(known["webhook_url"])
		if !strings.HasPrefix(u, "https://discord.com/api/webhooks/") && !strings.HasPrefix(u, "https://discordapp.com/api/webhooks/") {
			return errors.New("webhook_url must be a Discord webhook URL (https://discord.com/api/webhooks/…)")
		}
	case "telegram":
		if !telegramChat.MatchString(value(known["chat_id"])) {
			return errors.New("chat_id must be a numeric chat id or an @channel name")
		}
		if !telegramToken.MatchString(value(known["bot_token"])) {
			return errors.New("bot_token must be the token BotFather gave you (digits:letters)")
		}
	case "webhook":
		if err := checkURL(value(known["url"]), true); err != nil {
			return fmt.Errorf("url %w", err)
		}
	}
	return nil
}

// checkURL accepts an absolute http(s) URL with a host. A query string is
// allowed only where the whole URL is secret.
func checkURL(raw string, secret bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("must be an absolute http or https URL")
	}
	if !secret && (u.User != nil || u.RawQuery != "") {
		return errors.New("must not carry credentials or a query string (use the token field)")
	}
	return nil
}

// Target is how the dashboard names a channel's destination without a
// secret: the ntfy topic, the Telegram chat, the webhook's host.
func Target(kind string, config, secrets map[string]string) string {
	switch kind {
	case "ntfy":
		return strings.TrimRight(or(config["server"], DefaultNtfyServer), "/") + "/" + config["topic"]
	case "discord":
		return "Discord webhook"
	case "telegram":
		return "Telegram chat " + config["chat_id"]
	case "webhook":
		if u, err := url.Parse(secrets["url"]); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	return kind
}

var ntfyPriority = map[string]int{"info": 3, "warn": 4, "error": 5}

// Build makes the delivery request for one message.
func Build(ctx context.Context, kind string, config, secrets map[string]string, m Message) (*http.Request, error) {
	var target string
	var payload any
	header := http.Header{}
	switch kind {
	case "ntfy":
		// JSON publishing to the server root keeps a non-ASCII title out of
		// the headers.
		target = or(config["server"], DefaultNtfyServer)
		payload = map[string]any{"topic": config["topic"], "title": m.Title, "message": or(m.Body, m.Title),
			"priority": ntfyPriority[m.Severity], "tags": []string{"ezllm", m.Severity}}
		if t := secrets["token"]; t != "" {
			header.Set("Authorization", "Bearer "+t)
		}
	case "discord":
		target = secrets["webhook_url"]
		// No mentions: a title is never allowed to ping @everyone.
		payload = map[string]any{"content": clip("**"+m.Title+"**\n"+m.Body, 2000), "allowed_mentions": map[string]any{"parse": []string{}}}
	case "telegram":
		target = "https://api.telegram.org/bot" + secrets["bot_token"] + "/sendMessage"
		// Plain text: no parse_mode, so a title cannot inject markup.
		payload = map[string]any{"chat_id": config["chat_id"], "text": clip(m.Title+"\n\n"+m.Body, 4096), "disable_web_page_preview": true}
	case "webhook":
		target = secrets["url"]
		payload = m
		if t := secrets["bearer"]; t != "" {
			header.Set("Authorization", "Bearer "+t)
		}
	default:
		return nil, fmt.Errorf("unknown channel kind %q", kind)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("the channel's address is not a valid URL")
	}
	req.Header = header
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ezllm-notify")
	return req, nil
}

// Client is the delivery client: a 10 s budget per attempt and no redirects
// followed — a redirect would carry a bearer token to wherever it pointed.
func Client() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// Attempts and Backoff bound one delivery: three tries, 2 s then 6 s apart.
var (
	Attempts = 3
	Backoff  = []time.Duration{2 * time.Second, 6 * time.Second}
)

// Send delivers m to one channel, retrying a network failure, 429 or 5xx.
// The error is safe to store and show: it names the status or the failure,
// never a secret.
func Send(ctx context.Context, client *http.Client, kind string, config, secrets map[string]string, m Message, attempts int) error {
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return Redact(last, secrets)
			case <-time.After(Backoff[min(i-1, len(Backoff)-1)]):
			}
		}
		req, err := Build(ctx, kind, config, secrets, m)
		if err != nil {
			return Redact(err, secrets)
		}
		resp, err := client.Do(req)
		if err != nil {
			last = transportError(err)
			continue
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return nil
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			return errors.New("the channel answered with a redirect (" + strconv.Itoa(resp.StatusCode) + "), which is not followed")
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			last = errors.New("the channel answered " + strconv.Itoa(resp.StatusCode))
		default:
			return errors.New("the channel refused the message (" + strconv.Itoa(resp.StatusCode) + ")")
		}
	}
	return Redact(last, secrets)
}

// transportError drops the request URL a *url.Error carries (a Telegram
// token is in the path) and keeps only what failed.
func transportError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return errors.New("the channel did not answer within 10 s")
		}
		err = ue.Err
	}
	return errors.New("could not reach the channel: " + err.Error())
}

// Redact removes every secret value from err's text and bounds its length.
func Redact(err error, secrets map[string]string) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	for _, v := range secrets {
		if v = strings.TrimSpace(v); len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return errors.New(clip(s, 200))
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func or(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return b
}
