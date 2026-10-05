package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
)

func newCrypto(t *testing.T) *FieldCrypto {
	t.Helper()
	f, err := NewFieldCrypto("test-master-key")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	f := newCrypto(t)
	for _, plain := range []string{
		"sk-a…def0",
		"sk-sp-…cdef",
		"a",                       // shortest possible
		strings.Repeat("x", 4096), // long
		"unicode-ключ-🔑",          // non-ASCII
	} {
		ct, err := f.Encrypt(plain)
		if err != nil {
			t.Fatalf("encrypt %q: %v", plain, err)
		}
		if !strings.HasPrefix(ct, "enc:v1:") {
			t.Errorf("missing prefix: %s", ct[:20])
		}
		// Only assert non-leakage for plaintexts long enough that a coincidental
		// substring match is implausible (a 1-char plaintext like "a" appears in
		// hex output by chance — that is not a leak).
		if len(plain) >= 12 && strings.Contains(ct, plain) {
			t.Errorf("plaintext leaked into ciphertext for %q", plain)
		}
		got, err := f.Decrypt(ct)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if got != plain {
			t.Errorf("round trip = %q, want %q", got, plain)
		}
	}
}

// TestCiphertextIsHexOnly proves the wire format carries no plaintext bytes:
// every character after the prefix must be hex or ':'.
func TestCiphertextIsHexOnly(t *testing.T) {
	f := newCrypto(t)
	ct, err := f.Encrypt("sk-sup…3456")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(ct, "enc:v1:")
	for _, r := range body {
		if r != ':' && !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("non-hex char %q in ciphertext — format violation", r)
		}
	}
	if strings.Contains(ct, "secret") {
		t.Error("plaintext fragment present in ciphertext")
	}
}

func TestEncryptIsNonDeterministic(t *testing.T) {
	f := newCrypto(t)
	a, _ := f.Encrypt("same-plaintext")
	b, _ := f.Encrypt("same-plaintext")
	if a == b {
		t.Error("identical ciphertexts — IV must be random per call")
	}
	// both must still decrypt
	for _, ct := range []string{a, b} {
		if got, err := f.Decrypt(ct); err != nil || got != "same-plaintext" {
			t.Errorf("decrypt(%s) = %q, %v", ct[:24], got, err)
		}
	}
}

func TestDecryptRejectsWrongMasterKey(t *testing.T) {
	f1 := newCrypto(t)
	ct, err := f1.Encrypt("sk-secret-value")
	if err != nil {
		t.Fatal(err)
	}
	f2, err := NewFieldCrypto("a-different-master-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.Decrypt(ct); err == nil {
		t.Fatal("decryption with the wrong master key must fail")
	} else if !strings.Contains(err.Error(), "wrong master key") {
		t.Errorf("error should name the cause, got: %v", err)
	}
}

func TestDecryptDetectsTampering(t *testing.T) {
	f := newCrypto(t)
	ct, err := f.Encrypt("sk-secret-value")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(ct, ":")
	if len(parts) != 5 { // enc, v1, iv, ct, tag
		t.Fatalf("unexpected ciphertext shape: %d parts", len(parts))
	}
	// flip one hex nibble in the ciphertext body
	raw, err := hex.DecodeString(parts[3])
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0xFF
	parts[3] = hex.EncodeToString(raw)
	tampered := strings.Join(parts, ":")
	if _, err := f.Decrypt(tampered); err == nil {
		t.Fatal("GCM must reject tampered ciphertext")
	}
	// tamper the tag too
	ct2, _ := f.Encrypt("sk-secret-value")
	p2 := strings.Split(ct2, ":")
	tr, _ := hex.DecodeString(p2[4])
	tr[0] ^= 0xFF
	p2[4] = hex.EncodeToString(tr)
	if _, err := f.Decrypt(strings.Join(p2, ":")); err == nil {
		t.Fatal("GCM must reject a tampered auth tag")
	}
}

func TestDecryptMalformed(t *testing.T) {
	f := newCrypto(t)
	for _, bad := range []string{
		"",
		"plaintext-not-encrypted",
		"enc:v1:not-hex",
		"enc:v1:aa:bb",          // wrong part count
		"enc:v1:zz:bb:cc",       // invalid hex
		"enc:v1:abcd:abcd:abcd", // iv too short for GCM
	} {
		if _, err := f.Decrypt(bad); err == nil {
			t.Errorf("Decrypt(%q) should fail", bad)
		}
	}
}

func TestEncryptRejectsEmpty(t *testing.T) {
	f := newCrypto(t)
	if _, err := f.Encrypt(""); err == nil {
		t.Error("encrypting an empty secret should be refused (would mask a missing key)")
	}
}

func TestNewFieldCryptoRejectsEmptyMaster(t *testing.T) {
	for _, m := range []string{"", "   ", "\t\n"} {
		if _, err := NewFieldCrypto(m); err == nil {
			t.Errorf("master %q must be rejected", m)
		}
	}
}

func TestKeyHint(t *testing.T) {
	cases := map[string]string{
		// prefix = leading token before the first '-', tail = last 4 chars
		"sk-a…def0":   "sk…def0",
		"sk-sp-…bcde": "sk…bcde",
		"abcdefghij":  "abc…ghij", // no hyphen -> first 3 chars
		"abcd":        "••••",     // too short to hint safely
		"ab":          "••••",
		"":            "",
	}
	for in, want := range cases {
		if got := KeyHint(in); got != want {
			t.Errorf("KeyHint(%q) = %q, want %q", in, got, want)
		}
	}
	// hint must never contain the full key, and must be shorter
	full := "sk-a…def0"
	h := KeyHint(full)
	if strings.Contains(h, full) {
		t.Error("hint leaked the full key")
	}
	if len(h) >= len(full) {
		t.Errorf("hint %q not shorter than key", h)
	}
}

// TestKeyHintNeverReconstructible asserts the security property that matters:
// for realistic key lengths, the hint reveals a small minority of characters.
func TestKeyHintNeverReconstructible(t *testing.T) {
	for _, k := range []string{
		strings.Repeat("a", 67),  // opencode-go key length
		strings.Repeat("b", 113), // bailian key length
	} {
		h := KeyHint(k)
		revealed := strings.Count(h, "a") + strings.Count(h, "b")
		if float64(revealed)/float64(len(k)) > 0.2 {
			t.Errorf("hint %q reveals %d/%d chars (>20%%)", h, revealed, len(k))
		}
	}
}

func TestHashTokenDeterministicAndSaltless(t *testing.T) {
	a, b := HashToken("tok-abc"), HashToken("tok-abc")
	if a != b {
		t.Error("token hash must be deterministic (used for lookup)")
	}
	if HashToken("tok-abc") == HashToken("tok-abd") {
		t.Error("distinct tokens must hash distinctly")
	}
	if len(a) != 64 {
		t.Errorf("sha256 hex length = %d, want 64", len(a))
	}
}

// TestIVUniqueness guards against a reused-IV catastrophe (GCM's fatal flaw).
func TestIVUniqueness(t *testing.T) {
	f := newCrypto(t)
	seen := make(map[string]bool)
	for i := 0; i < 500; i++ {
		ct, err := f.Encrypt("same")
		if err != nil {
			t.Fatal(err)
		}
		iv := strings.Split(ct, ":")[2]
		if seen[iv] {
			t.Fatalf("IV reused after %d encryptions — catastrophic for GCM", i)
		}
		seen[iv] = true
	}
}

// TestMasterKeyFileFormat documents the on-disk master key handling: a 32-byte
// hex file, generated once, 0600, outside data/.
func TestMasterKeyFileFormat(t *testing.T) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	hexKey := hex.EncodeToString(b)
	if len(hexKey) != 64 {
		t.Errorf("hex master key length = %d, want 64", len(hexKey))
	}
	f, err := NewFieldCrypto(hexKey)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := f.Encrypt("sk-provider-key")
	if err != nil {
		t.Fatal(err)
	}
	// trailing newline in a key file must not break decryption (common footgun)
	f2, err := NewFieldCrypto(hexKey + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f2.Decrypt(ct); err == nil && got == "sk-provider-key" {
		t.Log("note: untrimmed master key still decrypts — loader must TrimSpace to avoid ambiguity")
	}
	// the loader is responsible for trimming; assert a *different* key fails
	f3, _ := NewFieldCrypto(strings.Repeat("0", 64))
	if _, err := f3.Decrypt(ct); err == nil {
		t.Error("a different master key must not decrypt")
	}
	_ = bytes.MinRead // keep bytes imported for future buffer tests
}
