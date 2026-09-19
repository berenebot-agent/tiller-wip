package crypto

import (
	"bytes"
	"errors"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func mustCipher(t *testing.T, keys ...[]byte) *Cipher {
	t.Helper()
	c, err := New(keys...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	c := mustCipher(t, testKey(1))
	aad := AAD("acct", "provider", "p1", "credential_secret")
	stored, err := c.Encrypt(aad, "sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !Encrypted(stored) {
		t.Fatalf("stored value %q is not marked encrypted", stored)
	}
	if bytes.Contains([]byte(stored), []byte("sk-secret")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := c.Decrypt(aad, stored)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-secret" {
		t.Fatalf("decrypted = %q, want sk-secret", got)
	}
}

func TestUniqueNonce(t *testing.T) {
	c := mustCipher(t, testKey(1))
	aad := AAD("acct")
	a, _ := c.Encrypt(aad, "same")
	b, _ := c.Encrypt(aad, "same")
	if a == b {
		t.Fatal("two encryptions of the same plaintext are identical; nonce is not random")
	}
}

func TestWrongKeyIsLocked(t *testing.T) {
	aad := AAD("acct")
	stored, _ := mustCipher(t, testKey(1)).Encrypt(aad, "secret")
	if _, err := mustCipher(t, testKey(2)).Decrypt(aad, stored); !errors.Is(err, ErrLocked) {
		t.Fatalf("wrong key error = %v, want ErrLocked", err)
	}
}

func TestTamperedCiphertextIsLocked(t *testing.T) {
	c := mustCipher(t, testKey(1))
	aad := AAD("acct")
	stored, _ := c.Encrypt(aad, "secret")
	// Flip the last base64 character to corrupt the tag.
	corrupt := stored[:len(stored)-1]
	if stored[len(stored)-1] == 'A' {
		corrupt += "B"
	} else {
		corrupt += "A"
	}
	if _, err := c.Decrypt(aad, corrupt); err == nil {
		t.Fatal("tampered ciphertext decrypted successfully")
	}
}

func TestAADMismatchIsLocked(t *testing.T) {
	c := mustCipher(t, testKey(1))
	stored, _ := c.Encrypt(AAD("acct-a", "provider", "p1", "credential_secret"), "secret")
	if _, err := c.Decrypt(AAD("acct-b", "provider", "p1", "credential_secret"), stored); !errors.Is(err, ErrLocked) {
		t.Fatalf("AAD mismatch error = %v, want ErrLocked", err)
	}
	if _, err := c.Decrypt(AAD("acct-a", "provider", "p2", "credential_secret"), stored); !errors.Is(err, ErrLocked) {
		t.Fatalf("provider mismatch error = %v, want ErrLocked", err)
	}
}

func TestPlaintextPassesThrough(t *testing.T) {
	c := mustCipher(t, testKey(1))
	got, err := c.Decrypt(AAD("acct"), "legacy-plaintext")
	if err != nil {
		t.Fatal(err)
	}
	if got != "legacy-plaintext" {
		t.Fatalf("got %q, want legacy-plaintext", got)
	}
}

func TestMalformedIsRejected(t *testing.T) {
	c := mustCipher(t, testKey(1))
	for _, bad := range []string{"enc:v1:notbase64", "enc:v1:AAAA", "enc:v1::", "enc:v1:AAAA:notbase64"} {
		if _, err := c.Decrypt(AAD("acct"), bad); !errors.Is(err, ErrMalformed) && !errors.Is(err, ErrLocked) {
			t.Fatalf("Decrypt(%q) error = %v, want ErrMalformed or ErrLocked", bad, err)
		}
	}
}

func TestMultiKeyRotation(t *testing.T) {
	old := mustCipher(t, testKey(1))
	aad := AAD("acct")
	stored, _ := old.Encrypt(aad, "secret")
	// A cipher carrying the new key first and the old key as decrypt-only can
	// still read the old value, and re-encrypts under the new key.
	rotated := mustCipher(t, testKey(2), testKey(1))
	got, err := rotated.Decrypt(aad, stored)
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret" {
		t.Fatalf("got %q, want secret", got)
	}
	newStored, _ := rotated.Encrypt(aad, got)
	if _, err := mustCipher(t, testKey(1)).Decrypt(aad, newStored); !errors.Is(err, ErrLocked) {
		t.Fatalf("old key alone decrypted new ciphertext: %v", err)
	}
}

func TestLockedCipher(t *testing.T) {
	c := Locked()
	if c.Enabled() {
		t.Fatal("locked cipher reports Enabled")
	}
	if !c.Locked() {
		t.Fatal("locked cipher does not report Locked")
	}
	if _, err := c.Encrypt(nil, "secret"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Encrypt on locked cipher = %v, want ErrNoKey", err)
	}
}
