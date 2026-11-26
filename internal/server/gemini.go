package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// geminiAuthed authenticates a Gemini-surface request. The key comes from the
// x-goog-api-key header (or Authorization: Bearer, like every other surface).
// The ?key= query form Google's API also accepts is refused unless Settings
// turns it on: a URL is written to proxy and browser logs, and the key with it.
func (s *Server) geminiAuthed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(r.Header.Get("x-goog-api-key"))
		q := r.URL.Query()
		if fromQuery := strings.TrimSpace(q.Get("key")); fromQuery != "" {
			// The key leaves the URL whatever happens next, so nothing
			// downstream can echo or log it.
			q.Del("key")
			r.URL.RawQuery = q.Encode()
			if token == "" {
				if s.settings == nil || !s.settings.Current().GeminiQueryKey {
					writeGeminiErr(w, http.StatusUnauthorized, "API keys in the URL (?key=) are turned off on this ezllm. Send the key in the x-goog-api-key header.")
					return
				}
				token = fromQuery
			}
		}
		if token == "" {
			s.inferAuthed(next)(w, r)
			return
		}
		if r2, ok := s.authenticate(w, r, token); ok {
			s.needRole("infer", next)(w, r2)
		}
	}
}

func writeGeminiErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": status, "message": msg, "status": geminiStatus(status)}})
}

// geminiModel is one model in Google's catalog shape.
func geminiModel(id string) map[string]any {
	return map[string]any{
		"name": "models/" + id, "baseModelId": id, "displayName": id,
		"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent", "countTokens"},
	}
}

// handleGeminiModels: GET /v1beta/models, every routable id in Google's shape.
func (s *Server) handleGeminiModels(w http.ResponseWriter, r *http.Request) {
	list, err := s.modelList(r.Context(), "")
	if err != nil {
		writeGeminiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	models := make([]map[string]any, 0, len(list))
	for _, m := range list {
		if id, ok := m["id"].(string); ok {
			models = append(models, geminiModel(id))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// handleGeminiModel: GET /v1beta/models/{model}.
func (s *Server) handleGeminiModel(w http.ResponseWriter, r *http.Request) {
	want := strings.TrimPrefix(r.PathValue("model"), "models/")
	list, err := s.modelList(r.Context(), "")
	if err != nil {
		writeGeminiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, m := range list {
		if m["id"] == want {
			writeJSON(w, http.StatusOK, geminiModel(want))
			return
		}
	}
	writeGeminiErr(w, http.StatusNotFound, fmt.Sprintf("model %q is not served here", want))
}

// handleGeminiAction: POST /v1beta/models/{model}:generateContent,
// :streamGenerateContent and :countTokens.
func (s *Server) handleGeminiAction(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	i := strings.LastIndex(rest, ":")
	if i <= 0 {
		writeGeminiErr(w, http.StatusNotFound, "use models/{model}:generateContent, :streamGenerateContent or :countTokens")
		return
	}
	model, action := strings.TrimPrefix(rest[:i], "models/"), rest[i+1:]
	switch action {
	case "generateContent", "streamGenerateContent", "countTokens":
	default:
		writeGeminiErr(w, http.StatusNotFound, fmt.Sprintf("method %q is not supported: use generateContent, streamGenerateContent or countTokens", action))
		return
	}
	liftDeadline := limitBodyRead(w, inferenceBodyTimeout)
	raw, tooLarge, err := readBody(r, s.maxBody)
	liftDeadline()
	if err != nil || tooLarge {
		status, msg := http.StatusBadRequest, "could not read body"
		if tooLarge {
			status, msg = http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d bytes", s.maxBody)
		}
		writeGeminiErr(w, status, msg)
		return
	}
	var g geminiRequest
	if err := json.Unmarshal(raw, &g); err != nil {
		writeGeminiErr(w, http.StatusBadRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if action == "countTokens" {
		s.geminiCountTokens(w, r, model, raw, g)
		return
	}
	stream := action == "streamGenerateContent"
	body, thoughts, err := geminiToChat(model, g, stream)
	if err != nil {
		if errors.Is(err, errGeminiUnsupported) {
			writeGeminiErr(w, http.StatusBadRequest, "not supported through ezllm: "+strings.TrimPrefix(err.Error(), errGeminiUnsupported.Error()+": "))
			return
		}
		writeGeminiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	chat, err := json.Marshal(body)
	if err != nil {
		writeGeminiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	inner := r.Clone(r.Context())
	inner.Method = http.MethodPost
	inner.URL.Path = "/v1/chat/completions"
	inner.Body = io.NopCloser(bytes.NewReader(chat))
	inner.ContentLength = int64(len(chat))
	inner.Header.Set("Content-Type", "application/json")
	inner.Header.Del("Content-Length")
	gw := &geminiWriter{w: w, header: http.Header{}, stream: stream, sse: r.URL.Query().Get("alt") == "sse",
		conv: &geminiStream{thoughts: thoughts}, thoughts: thoughts}
	s.inference(provider.SurfaceChat)(gw, inner)
	gw.finish()
}

// geminiCountTokens estimates the prompt's tokens. Gemini's countTokens takes
// contents directly or wrapped in generateContentRequest.
func (s *Server) geminiCountTokens(w http.ResponseWriter, r *http.Request, model string, raw []byte, g geminiRequest) {
	var wrapped struct {
		Req *geminiRequest `json:"generateContentRequest"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Req != nil && len(g.Contents) == 0 {
		g = *wrapped.Req
	}
	if !s.allowModel(w, r, clientName(r), model) {
		return
	}
	body, _, err := geminiToChat(model, g, false)
	if err != nil {
		writeGeminiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set(countTokensSourceHeader, "estimate")
	writeJSON(w, http.StatusOK, map[string]any{"totalTokens": estimatePromptTokens(body)})
}

// geminiWriter receives the chat path's answer and writes it to the client
// in Gemini's shape. A non-streamed answer or any error is buffered and
// converted whole; a stream is converted chunk by chunk, as SSE with
// ?alt=sse, otherwise as one JSON array written as it grows.
type geminiWriter struct {
	w        http.ResponseWriter
	header   http.Header
	status   int
	stream   bool
	sse      bool
	thoughts bool
	conv     *geminiStream

	buf     bytes.Buffer // the whole body when not streaming (or on error)
	line    []byte       // a partial SSE line between writes
	started bool         // headers sent to the client
	chunks  int          // Gemini chunks written
	live    bool         // the answer is an SSE stream being converted
}

func (g *geminiWriter) Header() http.Header { return g.header }

// Unwrap lets http.ResponseController reach the client connection (write
// deadlines); flushing stays here, where chunks are converted.
func (g *geminiWriter) Unwrap() http.ResponseWriter { return g.w }

func (g *geminiWriter) WriteHeader(code int) {
	if g.status == 0 {
		g.status = code
	}
}

func (g *geminiWriter) Write(p []byte) (int, error) {
	if g.status == 0 {
		g.status = http.StatusOK
	}
	if !g.stream || g.status >= 400 {
		return g.buf.Write(p)
	}
	if !g.live {
		if !strings.Contains(g.header.Get("Content-Type"), "event-stream") {
			return g.buf.Write(p) // a whole answer to a streamed request: one chunk at the end
		}
		g.live = true
	}
	data := append(g.line, p...)
	for {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			break
		}
		line := bytes.TrimRight(data[:nl], "\r")
		data = data[nl+1:]
		payload, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		for _, c := range g.conv.chunk(payload) {
			if err := g.emit(c); err != nil {
				return 0, err
			}
		}
	}
	g.line = append(g.line[:0], data...)
	return len(p), nil
}

func (g *geminiWriter) Flush() {
	if f, ok := g.w.(http.Flusher); ok && g.started {
		f.Flush()
	}
}

// start sends the client its headers: ezllm's own X-Ezllm-* headers pass
// through, the content type is Gemini's.
func (g *geminiWriter) start(status int, contentType string) {
	if g.started {
		return
	}
	g.started = true
	for k, v := range g.header {
		if strings.HasPrefix(strings.ToLower(k), "x-ezllm-") || strings.EqualFold(k, "Retry-After") {
			g.w.Header()[k] = v
		}
	}
	g.w.Header().Set("Content-Type", contentType)
	g.w.WriteHeader(status)
}

func (g *geminiWriter) emit(chunk map[string]any) error {
	b, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	if g.sse {
		g.start(http.StatusOK, "text/event-stream")
		out.WriteString("data: ")
		out.Write(b)
		out.WriteString("\r\n\r\n")
	} else {
		g.start(http.StatusOK, "application/json")
		if g.chunks == 0 {
			out.WriteString("[")
		} else {
			out.WriteString(",\r\n")
		}
		out.Write(b)
	}
	g.chunks++
	if _, err := g.w.Write(out.Bytes()); err != nil {
		return err
	}
	g.Flush()
	return nil
}

// finish writes whatever is still owed once the chat path has returned.
func (g *geminiWriter) finish() {
	status := g.status
	if status == 0 {
		status = http.StatusOK
	}
	switch {
	case status >= 400 && !g.started:
		g.start(status, "application/json")
		_ = json.NewEncoder(g.w).Encode(geminiError(status, g.buf.Bytes()))
	case !g.stream:
		resp, err := chatToGemini(g.buf.Bytes(), g.thoughts)
		if err != nil {
			g.start(http.StatusBadGateway, "application/json")
			_ = json.NewEncoder(g.w).Encode(geminiError(http.StatusBadGateway, []byte("the provider's answer could not be read")))
			return
		}
		g.start(http.StatusOK, "application/json")
		_ = json.NewEncoder(g.w).Encode(resp)
	default:
		if !g.live && g.buf.Len() > 0 {
			if resp, err := chatToGemini(g.buf.Bytes(), g.thoughts); err == nil {
				_ = g.emit(resp)
			}
		} else if len(bytes.TrimSpace(g.line)) > 0 {
			_, _ = g.Write([]byte("\n"))
		}
		if g.live {
			_ = g.emit(g.conv.final())
		}
		if g.chunks == 0 {
			_ = g.emit(g.conv.final())
		}
		if !g.sse {
			_, _ = g.w.Write([]byte("]"))
		}
		g.Flush()
	}
}
