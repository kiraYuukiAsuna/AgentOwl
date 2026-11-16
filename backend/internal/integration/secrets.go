package integration

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
)

type Vault struct{ aead cipher.AEAD }

func NewVault(key []byte) (*Vault, error) {
	if len(key) != 32 {
		return nil, errors.New("credential key must contain 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("invalid credential key")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: aead}, nil
}

// Bind aad to the owning connector/credential and key version to prevent swapping.
func (v *Vault) Seal(plaintext, aad []byte) (string, error) {
	if len(aad) == 0 {
		return "", errors.New("credential context is required")
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", errors.New("credential encryption failed")
	}
	sealed := v.aead.Seal(nonce, nonce, plaintext, aad)
	return "v1:" + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (v *Vault) Open(encoded string, aad []byte) ([]byte, error) {
	if !strings.HasPrefix(encoded, "v1:") || len(aad) == 0 {
		return nil, errors.New("invalid encrypted credential")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encoded, "v1:"))
	if err != nil || len(raw) < v.aead.NonceSize()+v.aead.Overhead() {
		return nil, errors.New("invalid encrypted credential")
	}
	plain, err := v.aead.Open(nil, raw[:v.aead.NonceSize()], raw[v.aead.NonceSize():], aad)
	if err != nil {
		return nil, errors.New("credential decryption failed")
	}
	return plain, nil
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*$`),
	regexp.MustCompile(`(?im)^[ \t]*(?:cookie|set-cookie|authorization|proxy-authorization)\s*:[^\r\n]+`),
	regexp.MustCompile(`(?i)https?://[^\s/@]+:[^\s/@]+@`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`),
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{16,})\b`),
	regexp.MustCompile(`\b(?:u-|t-)[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`(?i)(?:["']?)(?:token|access_token|refresh_token|tenant_access_token|user_access_token|app_secret|api_key|client_secret|authorization|cookie)(?:["']?)\s*[:=]\s*(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s&,;]+)`),
}

// Redact is a defense in depth filter; callers must still exclude unnecessary source data.
func Redact(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	for _, pattern := range secretPatterns {
		text = pattern.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}
