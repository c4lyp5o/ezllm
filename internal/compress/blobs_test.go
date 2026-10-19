package compress

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func b64Run(n int) string {
	raw := make([]byte, n)
	for i := range raw {
		raw[i] = byte(i*131 + 7)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func blobHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// A long base64 run or data: URI in tool output becomes a marker with its
// length and a short hash; the text around it is untouched, in every shape.
func TestBlobsReplaceBase64AndDataURIs(t *testing.T) {
	run := b64Run(1500)
	in := "uploaded ok\npayload=" + run + "\ndone"
	want := "uploaded ok\npayload=[ezllm/blob: " + itoaN(len(run)) + " chars base64, sha256 " + blobHash(run) + " elided]\ndone"
	sameInEveryShape(t, "blobs base64", toolOutputAfter(t, Stage{Engine: "blobs"}, "bash", in), want)

	uri := "data:image/png;base64," + run
	in = "<img src=\"" + uri + "\"> rendered"
	want = "<img src=\"[ezllm/blob: data:image/png;base64, " + itoaN(len(run)) + " chars, sha256 " + blobHash(run) + " elided]\"> rendered"
	sameInEveryShape(t, "blobs data uri", toolOutputAfter(t, Stage{Engine: "blobs"}, "deploy_status", in), want)
}

// Inside a JSON string the marker keeps the document valid: it carries no
// quote or backslash, and only the base64 value changes.
func TestBlobsKeepJSONValid(t *testing.T) {
	run := b64Run(1500)
	in := `{"name":"logo.png","bytes":"` + run + `","ok":true}`
	for shape, got := range toolOutputAfter(t, Stage{Engine: "blobs"}, "bash", in) {
		var v map[string]any
		if err := json.Unmarshal([]byte(got), &v); err != nil {
			t.Fatalf("%s: JSON no longer valid: %v", shape, err)
		}
		if v["name"] != "logo.png" || v["ok"] != true || !strings.HasPrefix(v["bytes"].(string), "[ezllm/blob: ") {
			t.Errorf("%s: wrong rewrite: %v", shape, v)
		}
	}
}

// Never: a file read, a run under min_bytes (default 1024), a hex digest
// (one character class), or ordinary text.
func TestBlobsLeaveFileReadsShortRunsAndHashesAlone(t *testing.T) {
	run := b64Run(1500)
	sameInEveryShape(t, "blobs file read", toolOutputAfter(t, Stage{Engine: "blobs"}, "read_file", run), run)
	short := b64Run(300)
	sameInEveryShape(t, "blobs short", toolOutputAfter(t, Stage{Engine: "blobs"}, "bash", short), short)
	sameInEveryShape(t, "blobs min_bytes", toolOutputAfter(t, Stage{Engine: "blobs", Options: map[string]any{"min_bytes": len(run) + 1}}, "bash", run), run)
	hexes := strings.Repeat("deadbeef0123456789abcdef", 80)
	sameInEveryShape(t, "blobs hex", toolOutputAfter(t, Stage{Engine: "blobs"}, "bash", hexes), hexes)
	prose := strings.Repeat("ordinary log text with words and spaces ", 60)
	sameInEveryShape(t, "blobs prose", toolOutputAfter(t, Stage{Engine: "blobs"}, "bash", prose), prose)
}

func itoaN(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
