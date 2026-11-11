package copilotbridge

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func imageFixture(t *testing.T, jpg bool) string {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			im.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var b bytes.Buffer
	var err error
	mime := "image/png"
	if jpg {
		err = jpeg.Encode(&b, im, nil)
		mime = "image/jpeg"
	} else {
		err = png.Encode(&b, im)
	}
	if err != nil {
		t.Fatal(err)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b.Bytes())
}
func imageRequest(urls []string, role, detail string) []byte {
	parts := []any{map[string]any{"type": "text", "text": "before"}}
	for _, url := range urls {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url, "detail": detail}}, map[string]any{"type": "text", "text": "after"})
	}
	b, _ := json.Marshal(map[string]any{"model": "gpt-6-luna", "messages": []any{map[string]any{"role": role, "content": parts}}})
	return b
}
func TestImagesBecomeOrderedNativeBlobsWithoutEnteringPrompt(t *testing.T) {
	pngURL, jpgURL := imageFixture(t, false), imageFixture(t, true)
	req, err := ParseRequest(imageRequest([]string{pngURL, jpgURL}, "user", "auto"))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Attachments) != 2 || req.Attachments[0].MIMEType != "image/png" || req.Attachments[1].MIMEType != "image/jpeg" || req.Attachments[0].Type != "blob" {
		t.Fatal("wrong native attachments")
	}
	if req.Attachments[0].Data != strings.SplitN(pngURL, ",", 2)[1] || req.Attachments[1].Data != strings.SplitN(jpgURL, ",", 2)[1] {
		t.Fatal("image bytes changed")
	}
	if !strings.Contains(req.Transcript, "before\n[image-1.png attached]\nafter\n[image-2.jpeg attached]\nafter") || strings.Contains(req.Prompt, req.Attachments[0].Data) {
		t.Fatal("image ordering lost or base64 leaked into prompt")
	}
	f := &fakeCLI{events: []string{`{"type":"assistant.message","data":{"content":"ok"}}`, evUsage, evIdle}}
	turn := ChannelTurn{Model: "gpt-6-luna", Prompt: req.Transcript, Attachments: req.Attachments}
	if _, err := f.drive(t, turn, nil); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	raw := append([]byte(nil), f.params["session.send"]...)
	f.mu.Unlock()
	var sent struct{ Attachments []ImageAttachment }
	if json.Unmarshal(raw, &sent) != nil || len(sent.Attachments) != 2 || sent.Attachments[0].Data != req.Attachments[0].Data {
		t.Fatal("images missing from session.send")
	}
}
func TestImagesRefuseUnsafeOrUnsupportedInputsWithoutEcho(t *testing.T) {
	good := imageFixture(t, false)
	raw, _ := base64.StdEncoding.DecodeString(strings.SplitN(good, ",", 2)[1])
	large := append(append([]byte(nil), raw...), make([]byte, maxImageBytes)...)
	hugePixels := append([]byte(nil), raw...)
	binary.BigEndian.PutUint32(hugePixels[16:20], 5000)
	binary.BigEndian.PutUint32(hugePixels[20:24], 5000)
	binary.BigEndian.PutUint32(hugePixels[29:33], crc32.ChecksumIEEE(hugePixels[12:29]))
	cases := map[string][]string{
		"remote": {"https://127.0.0.1/private-image-secret"}, "file": {"file:///private-image-secret"}, "bad data": {"data:image/png;base64,private-image-secret"},
		"empty": {"data:image/png;base64,"}, "unsupported MIME": {strings.Replace(good, "image/png", "image/gif", 1)}, "mismatch": {strings.Replace(good, "image/png", "image/jpeg", 1)},
		"too many": {good, good, good, good, good}, "too large": {"data:image/png;base64," + base64.StdEncoding.EncodeToString(large)}, "too many pixels": {"data:image/png;base64," + base64.StdEncoding.EncodeToString(hugePixels)},
	}
	for name, urls := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRequest(imageRequest(urls, "user", ""))
			if err == nil {
				t.Fatal("unsupported image accepted")
			}
			if strings.Contains(err.Error(), "private-image-secret") {
				t.Fatal("error echoed client data")
			}
		})
	}
	for _, role := range []string{"system", "developer", "assistant", "tool"} {
		if _, err := ParseRequest(imageRequest([]string{good}, role, "")); err == nil {
			t.Errorf("image accepted in %s", role)
		}
	}
	for _, detail := range []string{"low", "high", "invalid"} {
		if _, err := ParseRequest(imageRequest([]string{good}, "user", detail)); err == nil {
			t.Errorf("unsupported detail %s accepted", detail)
		}
	}
	padded := append(append([]byte(nil), raw...), make([]byte, 3<<20)...)
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(padded)
	if _, err := ParseRequest(imageRequest([]string{url, url, url}, "user", "")); err == nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Fatal("total image size limit not enforced")
	}
}
func TestImageHistoryKeepsMessageAssociation(t *testing.T) {
	req, err := ParseRequest(append([]byte(`{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":`+strconvQuote(imageFixture(t, false))+`}}]},{"role":"assistant","content":"previous answer"},{"role":"user","content":"Compare the earlier image."}]}`), nil...))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Attachments) != 1 || !strings.Contains(req.Transcript, "User: \n[image-1.png attached]\n\n\nAssistant: previous answer") {
		t.Fatal("history image association lost")
	}
}
func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
func TestVisionCannotFallBackToTextOnlyP(t *testing.T) {
	var plain int
	var prompt string
	s := &Server{Runner: refusingExec{&plain, &prompt}, Token: "t", Models: []Model{{ID: "gpt-6-luna"}}}
	for _, stream := range []bool{false, true} {
		var body map[string]any
		json.Unmarshal(imageRequest([]string{imageFixture(t, false)}, "user", ""), &body)
		body["stream"] = stream
		b, _ := json.Marshal(body)
		rec := postChat(s, string(b))
		if rec.Code == 200 || !strings.Contains(rec.Body.String(), "chat_tools_unsupported") {
			t.Fatalf("vision refusal not preserved: status=%d", rec.Code)
		}
	}
	if plain != 0 {
		t.Fatal("vision fell back to a text-only CLI invocation")
	}
}

const visionCatalogFixture = `{"models":[{"id":"gpt-6-luna","capabilities":{"supports":{"vision":true},"limits":{"vision":{"max_prompt_images":4,"max_prompt_image_size":4194304,"supported_media_types":["image/png","image/jpeg"]}}}}]}`

func TestImageModelCapabilitiesFailClosed(t *testing.T) {
	url := imageFixture(t, false)
	req, err := ParseRequest(imageRequest([]string{url}, "user", ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name              string
		vision            bool
		count, size       int
		mime, model, code string
	}{
		{"allowed", true, 1, 3 << 20, "image/png", "gpt-6-luna", ""},
		{"nonvision", false, 1, 3 << 20, "image/png", "gpt-6-luna", "vision_unsupported"},
		{"missing limits", true, 0, 0, "image/png", "gpt-6-luna", "vision_unsupported"},
		{"too small", true, 1, 1, "image/png", "gpt-6-luna", "invalid_image"},
		{"format refused", true, 1, 3 << 20, "image/jpeg", "gpt-6-luna", "invalid_image"},
		{"model absent", true, 1, 3 << 20, "image/png", "other-model", "vision_unsupported"},
		{"auto refused", true, 1, 3 << 20, "image/png", "auto", "vision_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := map[string]any{"models": []any{map[string]any{"id": "gpt-6-luna", "capabilities": map[string]any{"supports": map[string]any{"vision": tc.vision}, "limits": map[string]any{"vision": map[string]any{"max_prompt_images": tc.count, "max_prompt_image_size": tc.size, "supported_media_types": []string{tc.mime}}}}}}}
			b, _ := json.Marshal(map[string]any{"id": 1, "result": catalog})
			var writes bytes.Buffer
			c := &rpcConn{w: &writes, r: bufio.NewReader(strings.NewReader(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(b), b)))}
			err := c.validateImages(tc.model, req.Attachments)
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var f *Failure
				if !errors.As(err, &f) || f.Code != tc.code {
					t.Fatalf("error=%v", err)
				}
			}
			if strings.Contains(writes.String(), "session.send") {
				t.Fatal("capability check started inference")
			}
		})
	}
	c := &rpcConn{w: &bytes.Buffer{}, r: bufio.NewReader(strings.NewReader(``))}
	if c.validateImages("gpt-6-luna", req.Attachments) == nil {
		t.Fatal("unavailable catalog accepted")
	}
}
func TestImageRequestRejectsExcessModelImageCount(t *testing.T) {
	req, err := ParseRequest(imageRequest([]string{imageFixture(t, false), imageFixture(t, false)}, "user", ""))
	if err != nil {
		t.Fatal(err)
	}
	catalog := strings.Replace(visionCatalogFixture, `"max_prompt_images":4`, `"max_prompt_images":1`, 1)
	b := []byte(`{"id":1,"result":` + catalog + `}`)
	c := &rpcConn{w: &bytes.Buffer{}, r: bufio.NewReader(strings.NewReader(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(b), b)))}
	var f *Failure
	if err := c.validateImages("gpt-6-luna", req.Attachments); !errors.As(err, &f) || f.Code != "invalid_image" {
		t.Fatalf("error=%v", err)
	}
}
func TestImageUserMetadataIsIgnoredButMalformedAssistantIsRejected(t *testing.T) {
	c := &rpcConn{strict: true, session: "s1"}
	var msg rpcMessage
	if err := json.Unmarshal([]byte(`{"method":"session.event","params":{"sessionId":"s1","event":{"type":"user.message","data":{"content":[{"type":"image","data":"fixture"}]}}}}`), &msg); err != nil {
		t.Fatal(err)
	}
	if err := c.dispatch(msg); err != nil || len(c.queue) != 0 {
		t.Fatalf("metadata err=%v queue=%d", err, len(c.queue))
	}
	json.Unmarshal([]byte(`{"method":"session.event","params":{"sessionId":"s1","event":{"type":"assistant.message","data":{"content":[]}}}}`), &msg)
	if c.dispatch(msg) == nil {
		t.Fatal("malformed assistant event accepted")
	}
}
