// Package security contains API-key authentication, per-key rate limiting and
// the SSRF guard used for outbound fetches.
package security

import (
	"crypto/sha256"
	"crypto/subtle"
)

// Authenticator validates bearer tokens in constant time. When constructed with
// an empty key list authentication is disabled and every request is allowed.
type Authenticator struct {
	hashes  [][sha256.Size]byte
	enabled bool
}

// NewAuthenticator hashes the configured keys once at startup.
func NewAuthenticator(keys []string) *Authenticator {
	a := &Authenticator{}
	for _, k := range keys {
		k = trimSpace(k)
		if k == "" {
			continue
		}
		a.hashes = append(a.hashes, sha256.Sum256([]byte(k)))
	}
	a.enabled = len(a.hashes) > 0
	return a
}

// Enabled reports whether authentication is enforced.
func (a *Authenticator) Enabled() bool { return a != nil && a.enabled }

// Check compares the presented token against every configured key. Hashing
// first keeps the comparison length-independent, and subtle.ConstantTimeCompare
// prevents timing oracles.
func (a *Authenticator) Check(token string) bool {
	if !a.Enabled() {
		return true
	}
	if token == "" {
		return false
	}
	got := sha256.Sum256([]byte(token))
	ok := 0
	for i := range a.hashes {
		ok |= subtle.ConstantTimeCompare(got[:], a.hashes[i][:])
	}
	return ok == 1
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
