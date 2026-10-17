package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func geminiRoute(model string) Route {
	return Route{
		Account: provider.Account{
			Kind:    provider.KindGeminiOpenAI,
			BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		},
		Model: model,
	}
}

func TestGeminiAudioURLStripsOpenAIAndAppendsGenerateContent(t *testing.T) {
	got := geminiAudioURL(geminiRoute("models/gemini-2.5-flash-preview-tts"))
	want := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash-preview-tts:generateContent"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
}

func TestGeminiTTSRequestMapsOpenAIBody(t *testing.T) {
	body, err := geminiAudioRequest([]byte(`{"input":"Say: ok","voice":"alloy"}`), provider.SurfaceAudioTTS)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Contents []struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
		GenerationConfig struct {
			ResponseModalities []string `json:"responseModalities"`
			SpeechConfig       struct {
				VoiceConfig struct {
					PrebuiltVoiceConfig struct {
						VoiceName string `json:"voiceName"`
					} `json:"prebuiltVoiceConfig"`
				} `json:"voiceConfig"`
			} `json:"speechConfig"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload.Contents[0].Parts[0].Text; got != "Say: ok" {
		t.Fatalf("text = %q", got)
	}
	if got := payload.GenerationConfig.ResponseModalities; len(got) != 1 || got[0] != "AUDIO" {
		t.Fatalf("modalities = %v", got)
	}
	if got := payload.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName; got != "Kore" {
		t.Fatalf("alloy should map to Kore, got %q", got)
	}
}

func TestGeminiSTTRequestReadsMultipartAndInfersWAV(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "probe.wav")
	if err != nil {
		t.Fatal(err)
	}
	audio := []byte("RIFFdata")
	if _, err := part.Write(audio); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	body, err := geminiAudioRequest(buf.Bytes(), provider.SurfaceAudioSTT)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Contents []struct {
			Parts []struct {
				InlineData struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	inline := payload.Contents[0].Parts[1].InlineData
	if inline.MimeType != "audio/wav" {
		t.Fatalf("mime = %q, want audio/wav (multipart form files report application/octet-stream)", inline.MimeType)
	}
	decoded, err := base64.StdEncoding.DecodeString(inline.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, audio) {
		t.Fatalf("audio = %q, want %q", decoded, audio)
	}
}

func TestGeminiTTSResponseWrapsPCM(t *testing.T) {
	pcm := []byte{0, 0, 1, 0}
	raw, _ := json.Marshal(map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{map[string]any{
				"inlineData": map[string]any{
					"mimeType": "audio/L16;rate=24000",
					"data":     base64.StdEncoding.EncodeToString(pcm),
				},
			}}},
		}},
	})
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}
	if err := geminiAudioResponse(resp, provider.SurfaceAudioTTS); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	if len(out) < 44 || string(out[:4]) != "RIFF" || string(out[8:12]) != "WAVE" {
		t.Fatalf("expected a WAV header, got %q", out)
	}
	if got := resp.Header.Get("Content-Type"); got != "audio/wav" {
		t.Fatalf("content-type = %q", got)
	}
}

func TestGeminiSTTResponseReturnsOpenAIText(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"parts": []any{
				map[string]any{"text": "hello "},
				map[string]any{"text": "world"},
			}},
		}},
	})
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}
	if err := geminiAudioResponse(resp, provider.SurfaceAudioSTT); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	var got struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got.Text) != "hello world" {
		t.Fatalf("text = %q", got.Text)
	}
}
