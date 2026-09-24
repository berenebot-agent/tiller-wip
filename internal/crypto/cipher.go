// Package crypto provides authenticated encryption for recoverable tenant
// secrets (provider credentials, OAuth tokens, notification auth headers).
//
// Secrets are stored as versioned authenticated ciphertext in the form
//
//	enc:v1:<base64 nonce>:<base64 ciphertext+tag>
//
// The "v1" token is the storage-format version, fixed for now by the design of
// record (docs/roadmap_credential_encryption.md). AES-256-GCM is used with a
// fresh random nonce per value, so encrypting the same plaintext twice yields
// different ciphertext, and any tampering fails authentication.
//
// Every value is sealed with associated data (AAD) binding its security
// context — account, record kind, record id, and field — so a ciphertext
// cannot be moved between accounts, providers, or fields without detection.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const (
	// KeySize is the required master-key length in bytes (AES-256).
	KeySize = 32
	// prefix is the versioned storage marker for encrypted values.
	prefix = "enc:v1:"
	// nonceSize is the standard AES-GCM nonce length.
	nonceSize = 12
)

var (
	// ErrLocked is returned when an encrypted value cannot be decrypted with
	// any available key. It signals the locked state: the master key is
	// missing or does not match the one used to encrypt existing values.
	ErrLocked = errors.New("crypto: encrypted secret cannot be decrypted with the available key")
	// ErrMalformed is returned when a value carries the encrypted prefix but
	// is not a well-formed v1 ciphertext.
	ErrMalformed = errors.New("crypto: malformed encrypted value")
	// ErrNoKey is returned when encryption is attempted with no key.
	ErrNoKey = errors.New("crypto: no master key available")
)

// Cipher is an ordered key set: the first key encrypts, every key may decrypt.
// Keeping decrypt-only previous keys is what a master-key rotation needs. A
// Cipher with no keys is "locked": it can neither encrypt nor decrypt.
//
// A Cipher is safe for concurrent use.
type Cipher struct {
	aeads []cipher.AEAD
}

// New builds a Cipher from one or more raw 32-byte keys. keys[0] is the
// encryption key; the rest are decrypt-only.
func New(keys ...[]byte) (*Cipher, error) {
	if len(keys) == 0 {
		return nil, ErrNoKey
	}
	aeads := make([]cipher.AEAD, 0, len(keys))
	for i, key := range keys {
		if len(key) != KeySize {
			return nil, fmt.Errorf("crypto: key %d must be %d bytes, got %d", i, KeySize, len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		aeads = append(aeads, aead)
	}
	return &Cipher{aeads: aeads}, nil
}

// Locked returns a Cipher that holds no key. The store treats it as the locked
// state: secret reads and writes fail, and nothing can be encrypted.
func Locked() *Cipher { return &Cipher{} }

// Enabled reports whether the cipher can encrypt.
func (c *Cipher) Enabled() bool { return c != nil && len(c.aeads) > 0 }

// Locked reports whether the cipher holds no usable key.
func (c *Cipher) Locked() bool { return c == nil || len(c.aeads) == 0 }

// Encrypted reports whether s is a v1 encrypted value.
func Encrypted(s string) bool { return strings.HasPrefix(s, prefix) }

// Encrypt seals plaintext with associated data aad, returning the versioned
// storage form. The same plaintext encrypted twice yields different ciphertext.
func (c *Cipher) Encrypt(aad []byte, plaintext string) (string, error) {
	if c.Locked() {
		return "", ErrNoKey
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.aeads[0].Seal(nil, nonce, []byte(plaintext), aad)
	return prefix + base64.StdEncoding.EncodeToString(nonce) + ":" + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens a stored value. A value without the encrypted prefix is legacy
// plaintext and is returned unchanged. An encrypted value that no key can
// authenticate returns ErrLocked; a structurally invalid value returns
// ErrMalformed.
func (c *Cipher) Decrypt(aad []byte, stored string) (string, error) {
	if !Encrypted(stored) {
		return stored, nil
	}
	rest := strings.TrimPrefix(stored, prefix)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		return "", ErrMalformed
	}
	nonce, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil || len(nonce) != nonceSize {
		return "", ErrMalformed
	}
	sealed, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ErrMalformed
	}
	for _, aead := range c.aeads {
		plaintext, err := aead.Open(nil, nonce, sealed, aad)
		if err == nil {
			return string(plaintext), nil
		}
	}
	return "", ErrLocked
}

// AAD builds the associated data for a secret from its security context. Each
// part is length-prefixed so concatenation of adjacent values cannot collide.
func AAD(parts ...string) []byte {
	size := 0
	for _, p := range parts {
		size += 4 + len(p)
	}
	out := make([]byte, 0, size)
	var length [4]byte
	for _, p := range parts {
		binary.BigEndian.PutUint32(length[:], uint32(len(p)))
		out = append(out, length[:]...)
		out = append(out, p...)
	}
	return out
}
