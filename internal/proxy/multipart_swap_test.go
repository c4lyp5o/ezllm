package proxy

import (
	"bytes"
	"io"
	"mime/multipart"
	"testing"
)

func multipartUpload(t testing.TB, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "client-model")
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	_, _ = fw.Write(bytes.Repeat([]byte{0x5a}, size))
	_ = mw.WriteField("language", "en")
	_ = mw.Close()
	return buf.Bytes()
}

// The swap replaces only the model and carries the file through byte for byte.
func TestSwapModelMultipartKeepsTheFile(t *testing.T) {
	body := multipartUpload(t, 1<<20)
	out, err := swapModelMultipart(body, "upstream-model")
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(out), extractMultipartBoundary(body))
	got := map[string][]byte{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		got[p.FormName()] = b
	}
	if string(got["model"]) != "upstream-model" || string(got["language"]) != "en" || !bytes.Equal(got["file"], bytes.Repeat([]byte{0x5a}, 1<<20)) {
		t.Fatalf("rebuilt parts wrong: model=%q language=%q file=%d bytes", got["model"], got["language"], len(got["file"]))
	}
}

// BenchmarkSwapModelMultipart measures the rebuild of a 25 MiB upload.
func BenchmarkSwapModelMultipart(b *testing.B) {
	body := multipartUpload(b, 25<<20)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := swapModelMultipart(body, "upstream-model"); err != nil {
			b.Fatal(err)
		}
	}
}
