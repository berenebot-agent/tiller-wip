package hostedauth

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"
)

// TestAuthoritativeEmail pins the rule from Google's server-side verification
// guidance: Google is authoritative for gmail.com/googlemail.com addresses and
// for Workspace accounts (non-empty hd), and is NOT authoritative for a
// third-party address merely because email_verified is true. The distinction is
// what allows one-click linking of an existing account to be safe.
func TestAuthoritativeEmail(t *testing.T) {
	cases := []struct {
		name string
		gi   GoogleIdentity
		want bool
	}{
		{"gmail address", GoogleIdentity{Email: "user@gmail.com", EmailVerified: true}, true},
		{"gmail address mixed case", GoogleIdentity{Email: "User@Gmail.com", EmailVerified: true}, true},
		{"googlemail address", GoogleIdentity{Email: "user@googlemail.com", EmailVerified: true}, true},
		{"workspace address with hd", GoogleIdentity{Email: "user@corp.example", EmailVerified: true, HostedDomain: "corp.example"}, true},
		{"third-party address", GoogleIdentity{Email: "user@outlook.com", EmailVerified: true}, false},
		{"third-party address with lookalike suffix", GoogleIdentity{Email: "user@notgmail.com", EmailVerified: true}, false},
		{"third-party address with gmail in local part", GoogleIdentity{Email: "gmail@corp.example", EmailVerified: true}, false},
		{"gmail address but unverified", GoogleIdentity{Email: "user@gmail.com", EmailVerified: false}, false},
		{"workspace unverified", GoogleIdentity{Email: "user@corp.example", EmailVerified: false, HostedDomain: "corp.example"}, false},
		{"empty email", GoogleIdentity{EmailVerified: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.gi.AuthoritativeEmail(); got != tc.want {
				t.Fatalf("AuthoritativeEmail() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestValidateIDTokenCarriesHostedDomain proves the hd claim survives token
// validation, which is what makes the Workspace arm of the authority rule
// reachable in production.
func TestValidateIDTokenCarriesHostedDomain(t *testing.T) {
	const kid = "test-kid"
	verifier, key := testVerifier(t, kid)
	claims := baseClaims("not_provided")
	claims["hd"] = "corp.example"
	claims["email"] = "user@corp.example"
	token := signToken(t, key, kid, claims)

	got, err := verifier.ValidateIDToken(context.Background(), token, testGoogleClientID, "")
	if err != nil {
		t.Fatalf("ValidateIDToken() error = %v", err)
	}
	if got.HostedDomain != "corp.example" {
		t.Fatalf("HostedDomain = %q, want corp.example", got.HostedDomain)
	}
	if !got.AuthoritativeEmail() {
		t.Fatal("workspace identity not reported as authoritative")
	}
}

// TestValidateIDTokenThirdPartyNotAuthoritative is the production-shaped case:
// a real signed token for a third-party address must validate but report
// non-authoritative, so the link gate refuses it.
func TestValidateIDTokenThirdPartyNotAuthoritative(t *testing.T) {
	const kid = "test-kid"
	verifier, key := testVerifier(t, kid)
	claims := baseClaims("not_provided")
	claims["email"] = "user@outlook.com"
	token := signToken(t, key, kid, claims)

	got, err := verifier.ValidateIDToken(context.Background(), token, testGoogleClientID, "")
	if err != nil {
		t.Fatalf("ValidateIDToken() error = %v", err)
	}
	if got.AuthoritativeEmail() {
		t.Fatal("third-party email reported as authoritative")
	}
}

func TestPendingSignupPeekDoesNotConsume(t *testing.T) {
	store := NewPendingSignupStore()
	token, ok := store.Put(SignupClaims{Subject: "s1", Email: "user@example.com", Authoritative: false})
	if !ok {
		t.Fatal("Put failed")
	}

	peeked, ok := store.Peek(token)
	if !ok || peeked.Subject != "s1" {
		t.Fatalf("Peek = %+v ok=%v", peeked, ok)
	}
	// Peek must be repeatable.
	if _, ok := store.Peek(token); !ok {
		t.Fatal("second Peek failed; Peek consumed the claim")
	}
	// Authoritative flag must round-trip.
	if marked, _ := store.Put(SignupClaims{Subject: "s2", Email: "corp@example.com", Authoritative: true}); marked == "" {
		t.Fatal("Put with Authoritative failed")
	} else if peeked, ok := store.Peek(marked); !ok || !peeked.Authoritative {
		t.Fatalf("Authoritative did not round-trip: %+v ok=%v", peeked, ok)
	}

	if taken, ok := store.Take(token); !ok || taken.Subject != "s1" {
		t.Fatalf("Take = %+v ok=%v", taken, ok)
	}
	if _, ok := store.Peek(token); ok {
		t.Fatal("Peek succeeded after Take; Take did not consume")
	}
}

func TestPendingSignupPeekRejectsUnknownAndExpired(t *testing.T) {
	store := NewPendingSignupStore()
	if _, ok := store.Peek("no-such-token"); ok {
		t.Fatal("Peek accepted an unknown token")
	}
	if _, ok := store.Peek(""); ok {
		t.Fatal("Peek accepted an empty token")
	}

	token, ok := store.Put(SignupClaims{Subject: "expired", Email: "user@example.com"})
	if !ok {
		t.Fatal("Put failed")
	}
	// Age the entry past FlowTTL.
	key := sha256.Sum256([]byte(token))
	store.mu.Lock()
	claims := store.entries[key]
	claims.Expires = time.Now().Add(-time.Minute)
	store.entries[key] = claims
	store.mu.Unlock()

	if _, ok := store.Peek(token); ok {
		t.Fatal("Peek accepted an expired claim")
	}
	if _, ok := store.Take(token); ok {
		t.Fatal("Take accepted an expired claim")
	}
}
