// Package store owns ezllm's SQLite state: schema, migrations, field crypto,
// and the writer/reader connection split.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/scrypt"
)

// cryptoPrefix marks an encrypted field. Format: enc:v1:<iv-hex>:<ct-hex>:<tag-hex>
const cryptoPrefix = "enc:v1:"

// kdfSalt is a fixed application salt (the secret is the master key itself).
// Versioned in the string so a future re-key is unambiguous.
const kdfSalt = "ezllm-field-encryption-v1"

// ErrKeyFormat is returned when a ciphertext is malformed or fails authentication.
var ErrKeyFormat = errors.New("store: invalid ciphertext")

// FieldCrypto encrypts/decrypts provider API keys at rest with AES-256-GCM.
//
// The master key is NEVER stored in the database or in data/: it lives in a
// separate 0600 file (or env), so copying the DB alone leaks nothing.
type FieldCrypto struct {
	key []byte // 32 bytes, derived once at construction
}

// NewFieldCrypto derives the AES-256 key from the master secret via scrypt.
// Derivation happens once at startup (scrypt is deliberately slow).
func NewFieldCrypto(masterSecret string) (*FieldCrypto, error) {
	if strings.TrimSpace(masterSecret) == "" {
		return nil, errors.New("store: master key is empty")
	}
	// scrypt params: N=32768,r=8,p=1 -> ~250ms, 32 MiB. Fine once at boot.
	k, err := scrypt.Key([]byte(masterSecret), []byte(kdfSalt), 32768, 8, 1, 32)
	if err != nil {
		return nil, fmt.Errorf("store: derive key: %w", err)
	}
	return &FieldCrypto{key: k}, nil
}

// Encrypt returns "enc:v1:<iv>:<ct>:<tag>" (all hex).
func (f *FieldCrypto) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", errors.New("store: refusing to encrypt empty value")
	}
	block, err := aes.NewCipher(f.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", fmt.Errorf("store: read iv: %w", err)
	}
	// Seal appends the tag to the ciphertext; split them for the wire format.
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	tagSize := gcm.Overhead()
	if len(sealed) < tagSize {
		return "", ErrKeyFormat
	}
	ct, tag := sealed[:len(sealed)-tagSize], sealed[len(sealed)-tagSize:]
	return cryptoPrefix + hex.EncodeToString(iv) + ":" + hex.EncodeToString(ct) + ":" + hex.EncodeToString(tag), nil
}

// Decrypt reverses Encrypt. Returns ErrKeyFormat on malformed input and a
// distinct error on authentication failure (tamper / wrong master key).
func (f *FieldCrypto) Decrypt(encoded string) (string, error) {
	if !strings.HasPrefix(encoded, cryptoPrefix) {
		return "", ErrKeyFormat
	}
	parts := strings.Split(strings.TrimPrefix(encoded, cryptoPrefix), ":")
	if len(parts) != 3 {
		return "", fmt.Errorf("%w: expected 3 hex parts, got %d", ErrKeyFormat, len(parts))
	}
	iv, err := hex.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("%w: iv: %v", ErrKeyFormat, err)
	}
	ct, err := hex.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("%w: ct: %v", ErrKeyFormat, err)
	}
	tag, err := hex.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("%w: tag: %v", ErrKeyFormat, err)
	}
	block, err := aes.NewCipher(f.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(iv) != gcm.NonceSize() {
		return "", fmt.Errorf("%w: iv size %d != %d", ErrKeyFormat, len(iv), gcm.NonceSize())
	}
	// GCM expects ct||tag concatenated.
	plain, err := gcm.Open(nil, iv, append(ct, tag...), nil)
	if err != nil {
		return "", fmt.Errorf("store: decrypt failed (tampered, or wrong master key): %w", err)
	}
	return string(plain), nil
}

// KeyHint renders the only plaintext fragment we ever persist: 'sk-…cdef'.
// Never log or return the full key. Short values are masked rather than
// truncated, so a hint can never reconstruct its key.
func KeyHint(plain string) string {
	n := len(plain)
	if n == 0 {
		return ""
	}
	// Too short to hint safely: reveal nothing but the length class.
	if n <= 8 {
		return "••••"
	}
	// Prefix = leading token up to and including the first '-', else first 3 chars.
	prefixLen := 3
	if i := strings.Index(plain, "-"); i > 0 && i <= 8 {
		prefixLen = i // exclude the hyphen; we re-add the ellipsis
	}
	tail := plain[n-4:]
	// Refuse to emit a hint that exposes most of the key.
	if prefixLen+len(tail) >= n-1 {
		return "••••"
	}
	return plain[:prefixLen] + "…" + tail
}

// HashToken hashes a client token for storage/lookup (sha256 hex).
// Client tokens are not reversible; unlike provider keys we never need the
// plaintext again, so hashing beats encrypting.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
