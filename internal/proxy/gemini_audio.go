package proxy

// Native Gemini audio adapters.
//
// Google AI Studio's OpenAI-compatible endpoint serves chat/embeddings/images
// but NOT /audio/speech or /audio/transcriptions. Its native generateContent
// API does speak both — TTS returns inline base64 PCM, STT accepts inline
// audio and returns text — so these two surfaces need a request + response
// translation rather than a path swap. The client still sees the OpenAI wire.

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func isGeminiNativeAudio(rt Route, surface provider.Surface) bool {
	return rt.Account.Kind == provider.KindGeminiOpenAI &&
		(surface == provider.SurfaceAudioTTS || surface == provider.SurfaceAudioSTT)
}

func geminiAudioURL(rt Route) string {
	base := strings.TrimSuffix(strings.TrimSuffix(rt.Account.BaseURL, "/"), "/openai")
	return base + "/" + rt.Model + ":generateContent"
}

// geminiAudioRequest translates the OpenAI audio body into native
// generateContent JSON. TTS input is text; STT input is a multipart file.
func geminiAudioRequest(body []byte, surface provider.Surface) ([]byte, error) {
	if surface == provider.SurfaceAudioTTS {
		var in struct {
			Input string `json:"input"`
			Voice string `json:"voice"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("gemini tts body: %w", err)
		}
		voice := strings.TrimSpace(in.Voice)
		if voice == "" || voice == "alloy" {
			voice = "Kore"
		}
		payload := map[string]any{
			"contents": []any{map[string]any{"parts": []any{map[string]any{"text": in.Input}}}},
			"generationConfig": map[string]any{
				"responseModalities": []string{"AUDIO"},
				"speechConfig": map[string]any{
					"voiceConfig": map[string]any{
						"prebuiltVoiceConfig": map[string]any{"voiceName": voice},
					},
				},
			},
		}
		return json.Marshal(payload)
	}

	// STT: pull the uploaded file out of the OpenAI multipart envelope.
	boundary := extractMultipartBoundary(body)
	if boundary == "" {
		return nil, fmt.Errorf("gemini stt body: missing multipart boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	var audio []byte
	contentType := "audio/wav"
	var fileName string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gemini stt body: %w", err)
		}
		if part.FileName() != "" {
			fileName = part.FileName()
			audio, err = io.ReadAll(io.LimitReader(part, maxBodyBytes))
			if err != nil {
				return nil, fmt.Errorf("gemini stt body: %w", err)
			}
			if ct := part.Header.Get("Content-Type"); ct != "" && ct != "application/octet-stream" {
				contentType = ct
			}
			break
		}
	}
	if len(audio) == 0 {
		return nil, fmt.Errorf("gemini stt body: no audio file part")
	}
	if contentType == "" || contentType == "application/octet-stream" {
		if byExt := mime.TypeByExtension(filepath.Ext(fileName)); byExt != "" {
			contentType = byExt
		} else {
			contentType = "audio/wav"
		}
	}
	payload := map[string]any{
		"contents": []any{map[string]any{"parts": []any{
			map[string]any{"text": "Transcribe this audio exactly."},
			map[string]any{"inlineData": map[string]any{
				"mimeType": contentType,
				"data":     base64.StdEncoding.EncodeToString(audio),
			}},
		}}},
	}
	return json.Marshal(payload)
}

// geminiAudioResponse rewrites the native response back to the OpenAI shape:
// TTS -> audio bytes (PCM wrapped as WAV when needed), STT -> {"text": ...}.
func geminiAudioResponse(resp *http.Response, surface provider.Surface) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	var env struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text       string `json:"text"`
					InlineData struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	if len(env.Candidates) == 0 {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	if surface == provider.SurfaceAudioTTS {
		for _, p := range env.Candidates[0].Content.Parts {
			if p.InlineData.Data == "" {
				continue
			}
			audio, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
			if err != nil {
				return err
			}
			mimeType := p.InlineData.MimeType
			if strings.Contains(strings.ToLower(mimeType), "l16") || strings.Contains(strings.ToLower(mimeType), "pcm") {
				audio = pcmToWAV(audio, 24000, 1, 16)
				mimeType = "audio/wav"
			}
			if mimeType == "" {
				mimeType = "audio/wav"
			}
			resp.Body = io.NopCloser(bytes.NewReader(audio))
			resp.ContentLength = int64(len(audio))
			resp.Header.Set("Content-Type", mimeType)
			resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(audio)))
			return nil
		}
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	// STT: concatenate text parts.
	var text strings.Builder
	for _, p := range env.Candidates[0].Content.Parts {
		if p.Text != "" {
			text.WriteString(p.Text)
		}
	}
	out, _ := json.Marshal(map[string]any{"text": text.String()})
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(out)))
	return nil
}

// pcmToWAV wraps raw little-endian PCM in a canonical 44-byte WAV header.
func pcmToWAV(pcm []byte, sampleRate, channels, bits int) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(channels))
	_ = binary.Write(&b, binary.LittleEndian, uint32(sampleRate))
	byteRate := sampleRate * channels * bits / 8
	_ = binary.Write(&b, binary.LittleEndian, uint32(byteRate))
	blockAlign := channels * bits / 8
	_ = binary.Write(&b, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(&b, binary.LittleEndian, uint16(bits))
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}
