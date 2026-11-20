package bridgeutil

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestTailBufferKeepsTheLastFortyLines(t *testing.T) {
	var tb TailBuffer
	if tb.Tail() != "" {
		t.Fatal("empty buffer not empty")
	}
	for i := range 45 {
		tb.Add("l" + strconv.Itoa(i))
	}
	got := strings.Split(tb.Tail(), " | ")
	if len(got) != 40 || got[0] != "l5" || got[39] != "l44" {
		t.Fatalf("tail = %d lines, first %q last %q", len(got), got[0], got[len(got)-1])
	}
}

func TestWriteJSONBytesAndRandomHex(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSONBytes(rec, 418, []byte(`{"ok":true}`))
	if rec.Code != 418 || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("got %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if a, b := RandomHex(12), RandomHex(12); len(a) != 24 || a == b {
		t.Fatalf("RandomHex = %q, %q", a, b)
	}
}
