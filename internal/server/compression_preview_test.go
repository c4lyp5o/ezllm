package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type previewOut struct {
	Applied       bool   `json:"applied"`
	TokensBefore  int64  `json:"tokens_before"`
	TokensAfter   int64  `json:"tokens_after"`
	SavedNotional int64  `json:"saved_notional"`
	Note          string `json:"note"`
	After         string `json:"after"`
	Stages        []struct {
		Engine string `json:"engine"`
		Saved  int64  `json:"saved"`
	} `json:"stages"`
}

func preview(t *testing.T, h *harness, body string) (int, previewOut, string) {
	t.Helper()
	code, _, raw := h.do("POST", "/admin/compression-preview", "tok-admin", body)
	var out previewOut
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
	}
	return code, out, raw
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

// The preview runs the real engines on a pasted sample: a run of identical log
// lines folds to one line and a count, and the saving is reported per stage.
func TestCompressionPreviewRunsTheRealEngines(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	text := strings.Repeat("compiling module alpha ... ok\n", 80) + "done\n"
	body := `{"text":` + jsonString(text) + `,"tool":"terminal","profile":{"stages":[{"engine":"fold"}]}}`
	code, out, raw := preview(t, h, body)
	if code != http.StatusOK {
		t.Fatalf("code %d: %s", code, raw)
	}
	if !out.Applied || out.TokensAfter >= out.TokensBefore || len(out.After) >= len(text) {
		t.Fatalf("fold did not shrink the sample: %+v", out)
	}
	if len(out.Stages) != 1 || out.Stages[0].Engine != "fold" || out.Stages[0].Saved <= 0 {
		t.Fatalf("stage attribution = %+v, want one fold stage with a saving", out.Stages)
	}
}

// A saved profile previews by id, and a paused one still does: that is how a
// profile is judged before it is switched on.
func TestCompressionPreviewSavedAndPausedProfile(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	code, _, raw := h.do("POST", "/admin/compression-profiles", "tok-admin",
		`{"name":"p","enabled":false,"stages":[{"engine":"fold"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	var p struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.ID == 0 {
		t.Fatalf("create body %q: %v", raw, err)
	}
	text := strings.Repeat("retrying connection ... failed\n", 60) + "gave up\n"
	code, out, raw := preview(t, h, `{"profile_id":`+itoa(p.ID)+`,"text":`+jsonString(text)+`}`)
	if code != http.StatusOK || !out.Applied {
		t.Fatalf("paused profile did not preview: %d %s", code, raw)
	}
}

// Floors are the live behaviour by default; ignore_floors shows what the
// engines alone can do. A tiny saving under the ratio floor is declined and
// reported as notional, not applied.
func TestCompressionPreviewFloorsAndIgnoreFloors(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	text := strings.Repeat("x some filler text that stays as it is\n", 40) + "same\nsame\nsame\nsame\n"
	profile := `"profile":{"stages":[{"engine":"fold"}],"min_compress_ratio":0.9}`
	code, out, raw := preview(t, h, `{"text":`+jsonString(text)+`,`+profile+`}`)
	if code != http.StatusOK || out.Applied || out.After != text {
		t.Fatalf("a 0.9 floor must decline a small saving: %d %+v %s", code, out, raw)
	}
	if out.Note == "" {
		t.Error("a declined preview must say why")
	}
	code, out, raw = preview(t, h, `{"text":`+jsonString(text)+`,"ignore_floors":true,`+profile+`}`)
	if code != http.StatusOK || !out.Applied || len(out.After) >= len(text) {
		t.Fatalf("ignore_floors must show the engine's own result: %d %+v %s", code, out, raw)
	}
}

func TestCompressionPreviewRefusesBadInput(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	for name, body := range map[string]string{
		"empty text":      `{"text":"  ","profile":{"stages":[{"engine":"fold"}]}}`,
		"no profile":      `{"text":"hello"}`,
		"unknown engine":  `{"text":"hello","profile":{"stages":[{"engine":"nope"}]}}`,
		"no stages":       `{"text":"hello","profile":{"stages":[]}}`,
		"bad kind":        `{"text":"hello","kind":"image","profile":{"stages":[{"engine":"fold"}]}}`,
		"ratio out range": `{"text":"hello","profile":{"stages":[{"engine":"fold"}],"min_compress_ratio":1.5}}`,
		"missing saved":   `{"text":"hello","profile_id":99999}`,
	} {
		if code, _, raw := preview(t, h, body); code < 400 {
			t.Errorf("%s: code %d %s, want a 4xx", name, code, raw)
		}
	}
	if code, _, _ := preview(t, h, `{"text":"hello","profile":{"stages":[{"engine":"fold"}]}}`); code != http.StatusOK {
		t.Errorf("a valid tiny sample must still answer 200, got %d", code)
	}
	// The endpoint is admin-only.
	if code, _, _ := h.do("POST", "/admin/compression-preview", "", `{"text":"x"}`); code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Errorf("unauthenticated preview: code %d, want 401/403", code)
	}
}
