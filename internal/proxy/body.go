package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/translate"
)

// SwapModel rewrites the `model` field of a JSON request body, preserving every
// other field (unknown provider-specific fields such as extra_body, thinking,
// enable_thinking, tools, reasoning, previous_response_id): the new model is
// spliced into the client's own bytes, or the body is round-tripped through a
// map where a splice cannot normalize it (see jsonbody.SpliceModel).
func SwapModel(body []byte, newModel string) ([]byte, string, error) {
	if len(body) == 0 {
		return nil, "", errors.New("proxy: empty body")
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // avoid float64 mangling of large ints
	if err := dec.Decode(&m); err != nil {
		return nil, "", fmt.Errorf("proxy: body is not a JSON object: %w", err)
	}
	// `null`, `[]`, `"str"` etc. decode without error into a nil/non-object map;
	// assigning into a nil map panics, so reject explicitly.
	if m == nil {
		return nil, "", errors.New("proxy: body is not a JSON object")
	}
	old, _ := m["model"].(string)
	// The same splice-or-re-marshal choice jsonbody.WithModel makes: both
	// paths must emit identical bytes (docs/design/single-parse-dispatch-design.md).
	if out, ok := jsonbody.SpliceModel(body, newModel); ok {
		return out, old, nil
	}
	m["model"] = newModel
	out, err := json.Marshal(m)
	if err != nil {
		return nil, old, fmt.Errorf("proxy: re-marshal: %w", err)
	}
	return out, old, nil
}

// IsStream reports whether the body asks for streaming (without mutating it).
func IsStream(body []byte) bool {
	var m struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	return m.Stream
}

// isMultipartBody reports whether the body looks like a multipart/form-data
// payload (audio STT). Multipart bodies start with a boundary marker "--".
func isMultipartBody(body []byte) bool {
	return bytes.HasPrefix(body, []byte("--"))
}

// extractMultipartBoundary reads the boundary from the first line of a
// multipart body ("--<boundary>\r\n").
func extractMultipartBoundary(body []byte) string {
	eol := bytes.IndexAny(body, "\r\n")
	if eol < 0 {
		return ""
	}
	return strings.TrimPrefix(string(body[:eol]), "--")
}

// MultipartField reads one form field's value from a raw multipart body,
// using the same body-carried boundary the per-hop rewrite uses
// (extractMultipartBoundary), so what the handler reads and what each hop
// rewrites can never disagree. An absent field is ("", nil).
func MultipartField(body []byte, name string) (string, error) {
	if !bytes.ContainsAny(body, "\r\n") {
		return "", errors.New("multipart body has no boundary line")
	}
	boundary := extractMultipartBoundary(body)
	if boundary == "" {
		return "", errors.New("multipart body has empty boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("read multipart part: %w", err)
		}
		if part.FormName() == name {
			value, err := io.ReadAll(part)
			if err != nil {
				return "", fmt.Errorf("read %s field: %w", name, err)
			}
			return strings.TrimSpace(string(value)), nil
		}
	}
}

// swapModelMultipart rebuilds a multipart/form-data body with the "model" form
// field replaced by newModel. Every other part (the audio file, language,
// prompt, etc.) is copied through byte-for-byte. The boundary is extracted
// from the body's own first line, so no external Content-Type is needed.
func swapModelMultipart(body []byte, newModel string) ([]byte, error) {
	if !bytes.ContainsAny(body, "\r\n") {
		return nil, errors.New("proxy: multipart body has no boundary line")
	}
	boundary := extractMultipartBoundary(body)
	if boundary == "" {
		return nil, errors.New("proxy: multipart body has empty boundary")
	}
	// Use mime/multipart to read the parts, then rebuild with the model swapped.
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	out.Grow(len(body) + len(newModel) + 256) // one allocation for the rebuilt body
	mw := multipart.NewWriter(&out)
	if err := mw.SetBoundary(boundary); err != nil {
		return nil, fmt.Errorf("proxy: multipart boundary: %w", err)
	}
	var swapped bool
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("proxy: read multipart part: %w", err)
		}
		// Preserve the original part header (filename, content-type).
		h := textproto.MIMEHeader(part.Header)
		w, err := mw.CreatePart(h)
		if err != nil {
			return nil, fmt.Errorf("proxy: create multipart part: %w", err)
		}
		// Swap the model field; copy everything else straight through, so a
		// large upload is not held a second time as a separate part buffer.
		if part.FormName() == "model" {
			swapped = true
			if _, err := io.WriteString(w, newModel); err != nil {
				return nil, fmt.Errorf("proxy: write multipart part: %w", err)
			}
			continue
		}
		if _, err := io.Copy(w, part); err != nil {
			return nil, fmt.Errorf("proxy: read multipart part body: %w", err)
		}
	}
	if !swapped {
		// The request carried no model field at all: the handler resolved a
		// service default, so APPEND the model field this hop must carry
		// instead of refusing. A model-less multipart with no default is
		// refused before dispatch — injection only happens for a resolved one.
		if err := mw.WriteField("model", newModel); err != nil {
			return nil, fmt.Errorf("proxy: write multipart model field: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("proxy: close multipart writer: %w", err)
	}
	return out.Bytes(), nil
}

// isCloudflare reports whether the hop dials Cloudflare Workers AI, whose
// image and audio models take native bodies at native paths.
func isCloudflare(rt Route) bool {
	// The host, not a substring: api.cloudflare.com in another host's path is
	// not Cloudflare.
	u, err := url.Parse(rt.Account.BaseURL)
	return err == nil && strings.EqualFold(u.Hostname(), "api.cloudflare.com")
}

// prepareHopBody rewrites the client's body for one hop: a byte-stable model
// swap on the native path, or a full wire translation when the upstream wire
// differs from the client's. The WIRE comes from EffectiveWire — the same
// value attempt dials and commit unwraps — so a hop can never be dialed on one
// wire while its body is encoded for another. Translation REPLACES the swap:
// it rewrites the whole wire shape, model included, so the paths never compose.
func prepareHopBody(body []byte, rt Route, doc *jsonbody.BodyDoc) ([]byte, *effortChange, error) {
	// What THIS model accepts for reasoning_effort, from its own refusal. It
	// runs first so both the native swap and every translation below inherit
	// the clamped value — the client asked for a level this model names
	// differently, and the ledger's 400s are all this one field.
	//
	// A clamp invalidates the shared doc: it describes bytes we no longer send,
	// so the native path re-parses the clamped body instead of splicing the
	// model into the original. That costs one parse on the rare changed path,
	// and only a byte scan on a body that does not carry the field.
	var change *effortChange
	if clamped, c := clampReasoning(body, rt.Reasoning); c != nil {
		body, change, doc = clamped, c, nil
	}
	out, err := prepareHopBodyWire(body, rt, doc)
	return out, change, err
}

// prepareHopBodyWire is prepareHopBody without the reasoning clamp: the model
// swap and the wire translation. Split so the clamp can report what it changed
// without threading a return value through every branch below.
func prepareHopBodyWire(body []byte, rt Route, doc *jsonbody.BodyDoc) ([]byte, error) {
	// Google AI Studio native audio (TTS/STT) needs a generateContent
	// translation, not the OpenAI envelope. It runs before the multipart
	// branch because the STT input IS multipart and must be unwrapped.
	if isGeminiNativeAudio(rt, rt.Surface) {
		return geminiAudioRequest(body, rt.Surface)
	}
	// Cloudflare uses native TTS payloads: Deepgram Aura takes "text",
	// while MeloTTS takes "prompt", not the OpenAI input envelope.
	if rt.Surface == provider.SurfaceAudioTTS && isCloudflare(rt) {
		var in struct {
			Input string `json:"input"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("proxy: cloudflare tts body: %w", err)
		}
		text := in.Input
		if text == "" {
			text = in.Text
		}
		field := "prompt"
		if strings.HasPrefix(rt.Model, "@cf/deepgram/aura-") {
			field = "text"
		}
		return json.Marshal(map[string]any{field: text})
	}
	// Cloudflare flux-2 models require a multipart body (prompt as a form
	// field) rather than the JSON envelope. Build it here so the proxy's
	// content-type detection picks up the boundary.
	if rt.Surface == provider.SurfaceImagesGen && isCloudflare(rt) && strings.Contains(rt.Model, "flux-2") {
		var in struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("proxy: cloudflare image body: %w", err)
		}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if err := mw.WriteField("prompt", in.Prompt); err != nil {
			return nil, err
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	// Cloudflare Workers AI image models (non-flux-2) are native: the model is
	// in the URL and the JSON body accepts ONLY prompt-like fields.
	if rt.Surface == provider.SurfaceImagesGen && isCloudflare(rt) {
		var in struct {
			Prompt string `json:"prompt"`
			Seed   any    `json:"seed"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("proxy: cloudflare image body: %w", err)
		}
		out := map[string]any{"prompt": in.Prompt}
		if in.Seed != nil {
			out["seed"] = in.Seed
		}
		return json.Marshal(out)
	}
	// Multipart (audio STT): the model lives in a form field, not a JSON key,
	// and SwapModel is JSON-only. Rebuild the multipart body with the model
	// field swapped; everything else (the audio file part) passes through
	// byte-for-byte.
	if isMultipartBody(body) {
		return swapModelMultipart(body, rt.Model)
	}
	wire := rt.EffectiveWire()
	if wire != rt.Surface {
		// The three NEW cells ride the strict surface-neutral codecs; an
		// unsupported request feature is a typed refusal BEFORE any upstream
		// call (zero spend): the candidate loop skips the hop.
		if translate.IsUniversalCell(rt.Surface, wire) {
			return translate.RequestUniversal(body, rt.Surface, wire, rt.Model)
		}
		// Legacy cells keep their characterized converters.
		switch {
		case wire == provider.SurfaceChat:
			return translate.Request(body, rt.Model)
		case wire == provider.SurfaceResponses && rt.Surface == provider.SurfaceAnthropic:
			return translate.AnthropicToResponses(body, rt.Model)
		case wire == provider.SurfaceResponses && rt.Surface == provider.SurfaceChat:
			return translate.ChatToResponses(body, rt.Model)
		}
		// A cross-wire pair with no converter must never fall through to the
		// model swap: that would send the client's own shape to a wire that
		// cannot read it.
		return nil, fmt.Errorf("proxy: no request converter for %s client on the %s wire", rt.Surface, wire)
	}
	// Native: same shape on both ends, only the model id is swapped. When the
	// caller already decoded the body once (single-parse dispatch), mutate
	// that shared map instead of decoding again; a nil doc falls back to the
	// legacy self-contained decode. Both paths emit byte-identical bodies.
	if doc != nil {
		return doc.WithModel(rt.Model)
	}
	swapped, _, err := SwapModel(body, rt.Model)
	return swapped, err
}

// universalCodecOptions builds the strict codec options for a universal cell.
// When the UPSTREAM speaks Messages, the account's usage convention (schema
// v13) says what the upstream's own input_tokens means; auto reads as
// exclusive, as it does in the ledger.
func universalCodecOptions(rt Route) (translate.CodecOptions, error) {
	opts := translate.CodecOptions{}
	if rt.EffectiveWire() == provider.SurfaceAnthropic {
		conv, err := translate.UsageConventionOf(rt.Account.UsageConvention)
		if err != nil {
			return opts, err
		}
		opts.MessagesUsage = conv
	}
	return opts, nil
}
