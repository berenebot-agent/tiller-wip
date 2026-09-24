package auth

import (
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is the production cost for bcrypt-hashed machine tokens (client
// API keys and admin session tokens). Cost 10 is ~60 ms per verify on the
// deployment host: enough wall-clock work against offline guessing while
// keeping login latency and CPU bounded. Unlike argon2id it uses a fixed ~4 KiB
// working set, so it can never park 64 MiB per concurrent verify.
const bcryptCost = 10

// SecretHasher abstracts secret hashing so tests can inject a fast
// implementation while production uses the real KDF. The interface is
// intentionally small: Hash produces an encoded string, Verify checks a secret
// against one, and NeedsRehash reports whether an encoded hash should be
// upgraded to the hasher's current algorithm/parameters.
type SecretHasher interface {
	Hash(secret string) (string, error)
	Verify(secret, encoded string) bool
	// NeedsRehash reports whether encoded was produced by an older algorithm or
	// weaker parameters and should be re-hashed on next successful verify.
	NeedsRehash(encoded string) bool
}

// Argon2Hasher is the SecretHasher used for the low-entropy admin credential
// fingerprint, backed by Argon2id with the package's standard parameters
// (64 MiB, 3 iterations, 4 lanes). It delegates to the package-level
// HashSecret/VerifySecret, which hold the single Argon2id implementation.
type Argon2Hasher struct{}

// Hash implements SecretHasher by delegating to the production HashSecret.
func (Argon2Hasher) Hash(secret string) (string, error) {
	return argon2idHash(secret)
}

// Verify implements SecretHasher by delegating to the production VerifySecret.
func (Argon2Hasher) Verify(secret, encoded string) bool {
	return argon2idVerify(secret, encoded)
}

// NeedsRehash is always false: argon2id is the deliberate, current format for
// this hasher and is never downgraded.
func (Argon2Hasher) NeedsRehash(string) bool { return false }

// BcryptHasher is the production SecretHasher for high-entropy machine tokens
// (client API keys, admin session tokens). It hashes with bcrypt at Cost, and
// its Verify accepts both bcrypt hashes and legacy argon2id hashes so rows
// written before the tiering change keep working until lazy rehash upgrades
// them.
type BcryptHasher struct {
	Cost int
}

// Hash implements SecretHasher using bcrypt at h.Cost (bcryptCost when zero).
func (h BcryptHasher) Hash(secret string) (string, error) {
	cost := h.Cost
	if cost == 0 {
		cost = bcryptCost
	}
	encoded, err := bcrypt.GenerateFromPassword([]byte(secret), cost)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// Verify implements SecretHasher. A bcrypt encoding is checked with bcrypt; a
// legacy argon2id PHC string is checked with the argon2id verifier so existing
// keys and sessions survive the migration. Anything else is rejected.
func (h BcryptHasher) Verify(secret, encoded string) bool {
	return VerifyEncoded(secret, encoded)
}

// NeedsRehash reports true unless encoded is already a bcrypt hash at the
// hasher's current cost. Legacy argon2id hashes therefore rehash on next use.
func (h BcryptHasher) NeedsRehash(encoded string) bool {
	cost := h.Cost
	if cost == 0 {
		cost = bcryptCost
	}
	stored, err := bcrypt.Cost([]byte(encoded))
	if err != nil {
		return true
	}
	return stored != cost
}

// VerifyEncoded verifies secret against an encoded secret hash, dispatching on
// the encoding's algorithm prefix. It supports the current bcrypt format and
// the legacy argon2id PHC format so a deploy can verify pre-migration rows.
func VerifyEncoded(secret, encoded string) bool {
	switch {
	case strings.HasPrefix(encoded, "$argon2id$"):
		return argon2idVerify(secret, encoded)
	case strings.HasPrefix(encoded, "$2a$"), strings.HasPrefix(encoded, "$2b$"), strings.HasPrefix(encoded, "$2y$"):
		return bcrypt.CompareHashAndPassword([]byte(encoded), []byte(secret)) == nil
	default:
		return false
	}
}
