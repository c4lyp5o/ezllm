# OmniRoute API Reference Analysis for ezllm Implementation

**Generated:** 2026-10-13  
**Source:** https://github.com/diegosouzapw/OmniRoute/wiki/API-Reference  
**Current ezllm Version:** Based on CHANGELOG.md (latest 2026-10-13)

---

## 📊 Current ezllm Endpoint Coverage

### ✅ **Already Implemented (3/23 core compatibility endpoints)**

| Method | Path | Format | Status |
|--------|------|--------|--------|
| POST | `/v1/chat/completions` | OpenAI | ✅ `s.inference(SurfaceOpenAI)` |
| POST | `/v1/messages` | Anthropic | ✅ `s.inference(SurfaceAnthropic)` |
| POST | `/v1/responses` | OpenAI Responses | ✅ `s.inference(SurfaceResponses)` |
| GET | `/v1/models` | OpenAI | ✅ `s.handleModels` |

### 🔧 **Current Architecture**

**Surfaces defined in** `internal/provider/provider.go`:
```go
type Surface string
const (
    SurfaceOpenAI    Surface = "openai"    // POST /chat/completions
    SurfaceAnthropic Surface = "anthropic" // POST /messages
    SurfaceResponses Surface = "responses" // POST /responses
)
```

**Route registration** in `internal/server/server.go:routes()`:
- Lines 161-163: Core inference routes
- All go through `s.inference(surface)` handler

**Translation layer** in `internal/translate/`:
- `codec_chat.go` — OpenAI chat completions
- `codec_messages.go` — Anthropic messages
- `codec_responses.go` — OpenAI Responses
- `responses_request.go` + `responses_response.go` — Responses wire
- `anthropic_responses_*.go` — Anthropic-flavored Responses

---

## 🎯 **Missing Compatibility Endpoints (from OmniRoute's table)**

### **Tier 1: High Priority — Core API Parity**

| Method | Path | Format | Notes |
|--------|------|--------|-------|
| POST | `/v1/embeddings` | OpenAI | Embeddings generation |
| POST | `/v1/images/generations` | OpenAI Images | Image generation |
| POST | `/v1/images/edits` | OpenAI Images | Edit/inpaint (multipart) |
| POST | `/v1/audio/transcriptions` | OpenAI Audio | STT (multipart) |
| POST | `/v1/audio/speech` | OpenAI TTS | Returns audio body |
| GET | `/v1/models` | OpenAI | ✅ Already exists |

### **Tier 2: Medium Priority — Extended Modalities**

| Method | Path | Format | Notes |
|--------|------|--------|-------|
| POST | `/v1/videos/generations` | OpenAI-style video | Provider-prefixed model (e.g., `runway/gen-3`) |
| POST | `/v1/music/generations` | OpenAI-style music | Provider-prefixed model (e.g., `suno/v3.5`) |
| POST | `/v1/rerank` | Cohere/Voyage-style | Reranking endpoint |
| POST | `/v1/moderations` | OpenAI Moderations | Content moderation |

### **Tier 3: Client Compatibility — Alternative Wire Protocols**

| Method | Path | Format | Notes |
|--------|------|--------|-------|
| POST | `/v1/messages/count_tokens` | Anthropic | Token counting |
| GET | `/v1beta/models` | Gemini | Gemini model listing |
| POST | `/v1beta/models/{...path}` | Gemini | `generateContent` endpoint |
| POST | `/v1/api/chat` | Ollama | Ollama chat format |
| GET | `/api/tags` | Ollama | Ollama model tags |

### **Tier 4: Headerless/Tokenized Aliases — Integration Support**

| Method | Path | Format | Notes |
|--------|------|--------|-------|
| GET | `/api/v1/vscode/{token}/` | OpenAI | Catalog alias |
| GET | `/api/v1/vscode/{token}/models` | OpenAI | Models alias |
| POST | `/api/v1/vscode/{token}/chat/completions` | OpenAI | Tokenized chat |
| POST | `/api/v1/vscode/{token}/responses` | OpenAI | Tokenized responses |
| POST | `/api/v1/vscode/{token}/api/chat` | Ollama | Tokenized Ollama |
| GET | `/api/v1/vscode/{token}/api/tags` | Ollama | Tokenized tags |

### **Tier 5: Dedicated Provider Routes — Direct Provider Access**

| Method | Path | Format |
|--------|------|--------|
| POST | `/v1/providers/{provider}/chat/completions` | OpenAI |
| POST | `/v1/providers/{provider}/embeddings` | OpenAI |
| POST | `/v1/providers/{provider}/images/generations` | OpenAI |

---

## 🔧 **Implementation Requirements**

### **1. Extend Surface Type** (`internal/provider/provider.go`)

Add new surfaces:
```go
const (
    SurfaceOpenAI       Surface = "openai"       // POST /v1/chat/completions
    SurfaceAnthropic    Surface = "anthropic"    // POST /v1/messages
    SurfaceResponses    Surface = "responses"    // POST /v1/responses
    SurfaceEmbeddings   Surface = "embeddings"   // POST /v1/embeddings
    SurfaceImagesGen    Surface = "images_gen"   // POST /v1/images/generations
    SurfaceImagesEdit   Surface = "images_edit"  // POST /v1/images/edits
    SurfaceAudioSTT     Surface = "audio_stt"    // POST /v1/audio/transcriptions
    SurfaceAudioTTS     Surface = "audio_tts"    // POST /v1/audio/speech
    SurfaceVideoGen     Surface = "video_gen"    // POST /v1/videos/generations
    SurfaceMusicGen     Surface = "music_gen"    // POST /v1/music/generations
    SurfaceRerank       Surface = "rerank"       // POST /v1/rerank
    SurfaceModerations  Surface = "moderations"  // POST /v1/moderations
    SurfaceCountTokens  Surface = "count_tokens" // POST /v1/messages/count_tokens
    SurfaceGemini       Surface = "gemini"       // POST /v1beta/models/{...}
    SurfaceOllama       Surface = "ollama"       // POST /v1/api/chat
)
```

### **2. Add Upstream Paths** (`SurfacePath` function)

```go
func SurfacePath(s Surface) string {
    switch s {
    case SurfaceAnthropic:
        return "/messages"
    case SurfaceResponses:
        return "/responses"
    case SurfaceEmbeddings:
        return "/embeddings"
    case SurfaceImagesGen:
        return "/images/generations"
    case SurfaceImagesEdit:
        return "/images/edits"
    case SurfaceAudioSTT:
        return "/audio/transcriptions"
    case SurfaceAudioTTS:
        return "/audio/speech"
    case SurfaceVideoGen:
        return "/videos/generations"
    case SurfaceMusicGen:
        return "/music/generations"
    case SurfaceRerank:
        return "/rerank"
    case SurfaceModerations:
        return "/moderations"
    case SurfaceCountTokens:
        return "/messages/count_tokens"
    case SurfaceGemini:
        return "" // Special handling needed for /v1beta/models/{...}
    case SurfaceOllama:
        return "/api/chat"
    default:
        return "/chat/completions"
    }
}
```

### **3. Add Route Registrations** (`internal/server/server.go:routes()`)

```go
// Core inference endpoints
s.mux.HandleFunc("POST /v1/embeddings", s.authed(s.inference(provider.SurfaceEmbeddings)))
s.mux.HandleFunc("POST /v1/images/generations", s.authed(s.inference(provider.SurfaceImagesGen)))
s.mux.HandleFunc("POST /v1/images/edits", s.authed(s.inference(provider.SurfaceImagesEdit)))
s.mux.HandleFunc("POST /v1/audio/transcriptions", s.authed(s.inference(provider.SurfaceAudioSTT)))
s.mux.HandleFunc("POST /v1/audio/speech", s.authed(s.inference(provider.SurfaceAudioTTS)))
s.mux.HandleFunc("POST /v1/videos/generations", s.authed(s.inference(provider.SurfaceVideoGen)))
s.mux.HandleFunc("POST /v1/music/generations", s.authed(s.inference(provider.SurfaceMusicGen)))
s.mux.HandleFunc("POST /v1/rerank", s.authed(s.inference(provider.SurfaceRerank)))
s.mux.HandleFunc("POST /v1/moderations", s.authed(s.inference(provider.SurfaceModerations)))
s.mux.HandleFunc("POST /v1/messages/count_tokens", s.authed(s.inference(provider.SurfaceCountTokens)))

// Gemini compatibility
s.mux.HandleFunc("GET /v1beta/models", s.authed(s.handleGeminiModels))
s.mux.HandleFunc("POST /v1beta/models/{path...}", s.authed(s.inference(provider.SurfaceGemini)))

// Ollama compatibility
s.mux.HandleFunc("POST /v1/api/chat", s.authed(s.inference(provider.SurfaceOllama)))
s.mux.HandleFunc("GET /api/tags", s.authed(s.handleOllamaTags))

// Tokenized aliases (VS Code style)
s.mux.HandleFunc("GET /api/v1/vscode/{token}/", s.authed(s.handleVSCodeCatalog))
s.mux.HandleFunc("GET /api/v1/vscode/{token}/models", s.authed(s.handleVSCodeModels))
s.mux.HandleFunc("POST /api/v1/vscode/{token}/chat/completions", s.authed(s.inference(provider.SurfaceOpenAI)))
s.mux.HandleFunc("POST /api/v1/vscode/{token}/responses", s.authed(s.inference(provider.SurfaceResponses)))
s.mux.HandleFunc("POST /api/v1/vscode/{token}/api/chat", s.authed(s.inference(provider.SurfaceOllama)))
s.mux.HandleFunc("GET /api/v1/vscode/{token}/api/tags", s.authed(s.handleVSCodeOllamaTags))

// Dedicated provider routes
s.mux.HandleFunc("POST /v1/providers/{provider}/chat/completions", s.authed(s.providerInference(provider.SurfaceOpenAI)))
s.mux.HandleFunc("POST /v1/providers/{provider}/embeddings", s.authed(s.providerInference(provider.SurfaceEmbeddings)))
s.mux.HandleFunc("POST /v1/providers/{provider}/images/generations", s.authed(s.providerInference(provider.SurfaceImagesGen)))
```

### **4. Add Translation Codecs** (new files in `internal/translate/`)

| Surface | Request Codec | Response Codec | Notes |
|---------|--------------|----------------|-------|
| Embeddings | `codec_embeddings_request.go` | `codec_embeddings_response.go` | Simple JSON |
| Images Gen | `codec_images_request.go` | `codec_images_response.go` | Size, quality, format params |
| Images Edit | `codec_images_edit_request.go` | `codec_images_response.go` | Multipart handling |
| Audio STT | `codec_audio_stt_request.go` | `codec_audio_stt_response.go` | Multipart, language, timestamp |
| Audio TTS | `codec_audio_tts_request.go` | `codec_audio_tts_response.go` | Returns binary audio |
| Video Gen | `codec_video_request.go` | `codec_video_response.go` | Provider-prefixed models |
| Music Gen | `codec_music_request.go` | `codec_music_response.go` | Provider-prefixed models |
| Rerank | `codec_rerank_request.go` | `codec_rerank_response.go` | Query + documents array |
| Moderations | `codec_moderations_request.go` | `codec_moderations_response.go` | Input array, categories |
| Count Tokens | `codec_count_tokens_request.go` | `codec_count_tokens_response.go` | Anthropic format |
| Gemini | `codec_gemini_request.go` | `codec_gemini_response.go` | Parts-based content |
| Ollama | `codec_ollama_request.go` | `codec_ollama_response.go` | Different message format |

### **5. Adapter Support** (each adapter must handle new surfaces)

Check `PrepareRequest` in:
- `internal/provider/adapters.go` — `openCodeGo`, `openAICompatible`, `anthropicCompatible`

Each needs to handle new surfaces in their `PrepareRequest` method.

---

## 🏗️ **Implementation Strategy**

### **Phase 1: Core Extensions (Week 1-2)**
1. Add `SurfaceEmbeddings`, `SurfaceImagesGen`, `SurfaceAudioSTT`, `SurfaceAudioTTS` to provider.go
2. Add route registrations in server.go
3. Implement translation codecs for embeddings, images, audio
4. Update adapters to handle new surfaces
5. Test with existing providers (OpenAI-compatible, opencode-go)

### **Phase 2: Extended Modalities (Week 2-3)**
1. Add video, music, rerank, moderations surfaces
2. Implement codecs (simpler — mostly pass-through with validation)
3. Add provider-specific model routing (provider-prefixed models)

### **Phase 3: Compatibility Protocols (Week 3-4)**
1. Add Gemini (`v1beta`) endpoints
2. Add Ollama endpoints (`/v1/api/chat`, `/api/tags`)
3. Add tokenized VS Code aliases
4. Add dedicated provider routes

### **Phase 4: Polish & Testing (Week 4)**
1. Integration tests for all new endpoints
2. Dashboard support for new model types
3. Documentation updates
4. Load testing

---

## 🎯 **Quick Wins (Can Implement Today)**

These require **minimal new code** — mostly route registration + pass-through:

1. **`/v1/embeddings`** — Simple JSON in/out, same routing logic
2. **`/v1/images/generations`** — Pass-through with size/quality validation
3. **`/v1/audio/speech`** — TTS, returns audio body (streaming response)
4. **`/v1/moderations`** — Simple content check, boolean response
5. **`/v1/messages/count_tokens`** — Anthropic token counter
6. **Tokenized aliases** — Reuse existing handlers, just extract token from URL path

---

## ⚠️ **Complex Endpoints (Need Design Decisions)**

| Endpoint | Complexity | Decision Needed |
|----------|------------|-----------------|
| `/v1/images/edits` | Multipart upload | Handle file uploads in Go stdlib |
| `/v1/audio/transcriptions` | Multipart + streaming | File size limits, temp storage |
| `/v1/videos/generations` | Long-running | Async job pattern? Webhook? |
| `/v1/music/generations` | Long-running | Same as video |
| `/v1/rerank` | New wire format | Cohere vs Voyage schema diff |
| `/v1beta/*` (Gemini) | Different content model | Parts-based, not messages |
| `/v1/api/chat` (Ollama) | Different message format | `role` + `content` vs `messages` array |

---

## 📋 **Testing Checklist per Endpoint**

For each new endpoint:
- [ ] Route registered and reachable
- [ ] Auth middleware works (`s.authed`)
- [ ] Request body parsed/validated
- [ ] Model resolution works for surface
- [ ] Combo routing works (fallback, round-robin)
- [ ] Translation codec validates request
- [ ] Upstream request formed correctly
- [ ] Response translated back to client format
- [ ] Ledger entry written with correct surface
- [ ] Streaming works (if applicable)
- [ ] Error handling returns proper format
- [ ] Dashboard shows in request logs

---

## 🔗 **Reference: OmniRoute Schema Files (for validation)**

From the wiki — schemas in `src/shared/validation/schemas.ts`:
- `v1RerankSchema`
- `v1ModerationSchema`
- `v1AudioSpeechSchema`
- `v1BatchCreateSchema`
- `v1SearchSchema`

We should mirror these validation rules.

---

## 💡 **Recommendation**

**Start with Phase 1 quick wins** — they unblock the most client integrations (embeddings for RAG, images for UI, audio for voice apps) and reuse 90% of existing inference pipeline.

The **translation layer** is the main investment — each new surface needs request/response codec. But the pattern is established: classify keys → build IR → translate per provider → translate back.

**Priority order for Calypso:**
1. Embeddings (RAG pipelines need this)
2. Images gen (UI generation)
3. Audio TTS/STT (voice interfaces)
4. Tokenized aliases (VS Code / Copilot integration)
5. Ollama/Gemini (client compatibility)
6. Video/Music/Rerank/Moderations (niche, can wait)

---

*Analysis complete. Ready to implement when you give the word, babe! 💕*