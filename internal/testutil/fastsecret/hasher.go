// Package fastsecret provides a test-only SecretHasher. It is intentionally
// NOT cryptographically suitable for production: it uses a single SHA-256
// with a deterministic per-secret salt, so hashing is cheap and repeatable.
// Importing this package into production code would be a bug — there is no
// runtime switch that selects it, and the encoding prefix makes its output
// visibly distinct from real Argon2id hashes.
package fastsecret

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"

	"github.com/tiller-router/tiller-router/internal/auth"
)

// Encoding prefix marks output as test-only. Production Argon2id hashes never
// start with this prefix, so a stray fast hash in a production DB is obvious.
const prefix = "test-sha256:"

// Hasher is a fast, deterministic SecretHasher for tests only.
type Hasher struct{}

// Hash returns a cheap, deterministic, non-production encoding of secret.
// The output is NOT a secure password hash: it exists only to let tests
// exercise SessionStore/ClientAuthenticator semantics without paying the
// 64 MiB Argon2id cost on every operation.
func (Hasher) Hash(secret string) (string, error) {
	h := sha256.Sum256([]byte(secret))
	return prefix + hex.EncodeToString(h[:]), nil
}

// Verify checks secret against a fast-secret encoded string. Returns false for
// anything that does not use the test prefix (including real Argon2id hashes),
// so it can never be fooled into accepting a production hash as valid.
func (Hasher) Verify(secret, encoded string) bool {
	if len(encoded) < len(prefix) || encoded[:len(prefix)] != prefix {
		return false
	}
	h := sha256.Sum256([]byte(secret))
	want, err := hex.DecodeString(encoded[len(prefix):])
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(want, h[:]) == 1
}

// Compile-time guard: Hasher must satisfy the interface.
var _ auth.SecretHasher = Hasher{}
