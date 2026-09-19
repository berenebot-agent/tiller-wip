package auth

import (
	"strings"
	"testing"
)

func TestGenerateAndVerifyKey(t *testing.T) {
	generated, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(generated.Plaintext, "sk-tr-") {
		t.Fatalf("unexpected prefix: %s", generated.Plaintext)
	}
	selector, secret, ok := ParseKey(generated.Plaintext)
	if !ok || selector != generated.Selector {
		t.Fatal("generated key did not parse")
	}
	if !VerifyEncoded(secret, generated.Hash) {
		t.Fatal("secret did not verify")
	}
	if VerifyEncoded(secret+"x", generated.Hash) {
		t.Fatal("wrong secret verified")
	}
	// Client keys are high-entropy machine tokens and use the production token
	// hasher (bcrypt), not the memory-hard credential hasher.
	if !strings.HasPrefix(generated.Hash, "$2") {
		t.Fatalf("expected bcrypt hash, got %q", generated.Hash)
	}
	if (BcryptHasher{}).NeedsRehash(generated.Hash) {
		t.Fatal("freshly generated bcrypt key unexpectedly needs rehash")
	}
	if strings.Contains(generated.Hash, secret) {
		t.Fatal("hash contains plaintext secret")
	}
}

func TestParseKeyRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"", "sk-tr-a.b", "Bearer sk-tr-abc.def", "sk-ordinary"} {
		if _, _, ok := ParseKey(value); ok {
			t.Fatalf("accepted malformed key %q", value)
		}
	}
}

func FuzzParseKey(f *testing.F) {
	f.Add("sk-tr-selector.secret")
	f.Add("")
	f.Fuzz(func(t *testing.T, value string) {
		selector, secret, ok := ParseKey(value)
		if ok && (len(selector) != 12 || len(secret) != 43) {
			t.Fatalf("accepted invalid lengths: %d/%d", len(selector), len(secret))
		}
	})
}
