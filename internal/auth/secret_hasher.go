package auth

// SecretHasher abstracts secret hashing so tests can inject a fast
// implementation while production uses Argon2id. The interface is
// intentionally small: Hash produces an encoded string, Verify checks a
// secret against one.
type SecretHasher interface {
	Hash(secret string) (string, error)
	Verify(secret, encoded string) bool
}

// Argon2Hasher is the production SecretHasher, backed by Argon2id with the
// package's standard parameters (64 MiB, 3 iterations, 4 lanes). It delegates
// to the package-level HashSecret/VerifySecret, which hold the single
// Argon2id implementation.
type Argon2Hasher struct{}

// Hash implements SecretHasher by delegating to the production HashSecret.
func (Argon2Hasher) Hash(secret string) (string, error) {
	return argon2idHash(secret)
}

// Verify implements SecretHasher by delegating to the production VerifySecret.
func (Argon2Hasher) Verify(secret, encoded string) bool {
	return argon2idVerify(secret, encoded)
}
