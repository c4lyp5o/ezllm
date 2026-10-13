package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// TestNewInferenceRoutesRegistered verifies the new Phase 1 endpoints reach the
// inference handler (proven by our JSON error envelope with the right surface).
// A route that was NOT registered would fall through to the SPA catch-all and
// answer HTML, never this shape.
func TestNewInferenceRoutesRegistered(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path:      filepath.Join(dir, "t.sqlite"),
		BatchSize: 4, BatchWait: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.UpsertClientToken(context.Background(), "hermes", "tok-infer", "infer"); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	registry := provider.NewRegistry(client)
	dispatcher := proxy.NewDispatcher(client, registry)
	s := New(Options{
		Auth: db, Resolver: router.New(db),
		Dispatcher: dispatcher,
		DB:         db, MaxBodyMiB: 1,
		Registry: registry, Tester: registration.NewTester(client, registry, db),
	})

	// No account registered, so every route is expected to answer OUR
	// model_not_found envelope (404) — which is how we know the handler ran.
	routes := map[string]struct {
		method       string
		body         *strings.Reader
		contentType  string
		expectStatus int
		expectError  string // empty = don't check
	}{
		"/v1/embeddings": {
			method:       http.MethodPost,
			body:         strings.NewReader(`{"model":"test/gpt-6-luna","input":"test"}`),
			contentType:  "application/json",
			expectStatus: http.StatusNotFound,
			expectError:  "model_not_found",
		},
		"/v1/images/generations": {
			method:       http.MethodPost,
			body:         strings.NewReader(`{"model":"test/gpt-6-luna","prompt":"test"}`),
			contentType:  "application/json",
			expectStatus: http.StatusNotFound,
			expectError:  "model_not_found",
		},
		"/v1/audio/speech": {
			method:       http.MethodPost,
			body:         strings.NewReader(`{"model":"test/tts-1","input":"test"}`),
			contentType:  "application/json",
			expectStatus: http.StatusNotFound,
			expectError:  "model_not_found",
		},
		"/v1/audio/transcriptions": {
			method:       http.MethodPost,
			body:         multipartBody(map[string]string{"model": "test/whisper-1"}, "audio.wav", "test audio data"),
			contentType:  "multipart/form-data; boundary=testboundary123",
			expectStatus: http.StatusNotFound,
			expectError:  "model_not_found",
		},
	}
	for route, spec := range routes {
		req := httptest.NewRequest(spec.method, route, spec.body)
		req.Header.Set("Authorization", "Bearer tok-infer")
		req.Header.Set("Content-Type", spec.contentType)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != spec.expectStatus {
			t.Errorf("%s: expected %d, got %d — %q", route, spec.expectStatus, rec.Code, rec.Body.String())
			continue
		}

		if spec.expectError != "" {
			var env struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Errorf("%s: body is not our JSON error envelope: %q", route, rec.Body.String())
				continue
			}
			if env.Error.Type != spec.expectError {
				t.Errorf("%s: expected error type %q, got %q", route, spec.expectError, env.Error.Type)
				continue
			}
		}
		t.Logf("%s (%s): %d OK", route, spec.method, rec.Code)
	}
}

// multipartBody builds a minimal multipart/form-data body for testing.
func multipartBody(fields map[string]string, filename, fileContent string) *strings.Reader {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write([]byte(fileContent))
	_ = mw.Close()
	return strings.NewReader(buf.String())
}
