package http

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// SignatureHeader carries the HMAC-SHA256 signature of the request body.
const SignatureHeader = "X-Hub-Signature-256"

// VerifySignature reports whether header ("sha256=<hex>") is the HMAC-SHA256 of
// body keyed with any one of secrets, so deliveries signed with an old or a new
// secret both pass while the App's secret is rotated. Every secret is tried,
// each compared in constant time; an empty secret never verifies.
func VerifySignature(secrets [][]byte, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(header[len(prefix):])
	if err != nil || len(got) != sha256.Size {
		return false
	}
	ok := false
	for _, secret := range secrets {
		if len(secret) == 0 {
			continue
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		if hmac.Equal(got, mac.Sum(nil)) {
			ok = true
		}
	}
	return ok
}

// Sign returns the X-Hub-Signature-256 header value for body (used by tests and tools).
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
