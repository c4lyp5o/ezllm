package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// failingBody fails every Read, so geminiAudioResponse returns before it
// reaches its own Close.
type failingBody struct{ closes *atomic.Int32 }

func (b *failingBody) Read([]byte) (int, error) { return 0, errors.New("upstream body read failed") }
func (b *failingBody) Close() error {
	b.closes.Add(1)
	return nil
}

type failingTransport struct{ closes *atomic.Int32 }

func (t *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     h,
		Body:       &failingBody{closes: t.closes},
		Request:    &http.Request{},
	}, nil
}

// A Google AI Studio native audio response that cannot be read is discarded:
// attempt returns a nil response, so the body it opened must be closed here.
// geminiAudioResponse only closes the body AFTER a successful read, so the
// read-failure path used to hand back a nil response with a live body.
func TestGeminiAudioReadFailureClosesBody(t *testing.T) {
	var closes atomic.Int32
	d := NewDispatcher(
		&http.Client{Transport: &failingTransport{closes: &closes}, Timeout: 5 * time.Second},
		provider.NewRegistry(&http.Client{Timeout: 5 * time.Second}))

	rt := Route{
		Account: provider.Account{
			ID: 1, Name: "gemini", Namespace: "gemini", Kind: provider.KindGeminiOpenAI,
			BaseURL: "https://generativelanguage.googleapis.com", Enabled: true,
		},
		KeyID: 1, KeyPlain: "test-key", KeyHint: "test…key",
		Model: "gemini-2.5-flash-tts", Alias: "gemini/tts", Surface: provider.SurfaceAudioTTS,
	}

	req := httptest.NewRequest("POST", "/v1/audio/speech", nil)
	req.Header.Set("Content-Type", "application/json")

	resp, _, _, err := d.attempt(context.Background(), req, rt, []byte(`{"input":"hi"}`), nil)
	if err == nil {
		t.Fatalf("expected the read failure to surface, got nil")
	}
	if resp != nil {
		t.Fatalf("a failed rewrite must not return a response, got %d", resp.StatusCode)
	}
	if n := closes.Load(); n != 1 {
		t.Fatalf("the discarded response body must be closed exactly once, got %d closes", n)
	}
}
