package auth

import (
	"context"
	"testing"
	"time"
)

// TestVerifySecretContextGivesUpWhenCallerIsGone proves a request that has
// already ended never parks on the Argon admission gate while the gate is busy,
// and that a caller who went away is never reported as a wrong password. (With a
// free slot the check simply runs; the contract is "never parked, never
// misreported", not "never executed".)
func TestVerifySecretContextGivesUpWhenCallerIsGone(t *testing.T) {
	t.Parallel()
	encoded, err := HashSecret("the right password")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, available := VerifySecretContext(ctx, "the right password", encoded); available && !ok {
		t.Fatal("a cancelled caller must never be reported as a mismatch")
	}
	// Everything still works for a live caller.
	if ok, available := VerifySecretContext(context.Background(), "the right password", encoded); !ok || !available {
		t.Fatalf("live verification: ok=%v available=%v, want both true", ok, available)
	}
}

// TestVerifySecretContextDoesNotParkBehindSaturatedGate proves the wait is
// bounded by the caller's own lifetime: with every slot held, a live caller
// queues (as designed) while a cancelled one returns immediately.
func TestVerifySecretContextDoesNotParkBehindSaturatedGate(t *testing.T) {
	// Deliberately not t.Parallel: this test swaps the process-wide admission
	// gate, and another test that does the same must not overlap it.
	encoded, err := HashSecret("the right password")
	if err != nil {
		t.Fatal(err)
	}
	// Shrink the gate to one slot and hold it. Shrinking (rather than filling
	// the production gate) keeps this test from starving other tests in the
	// package, which queue behind the same slots.
	restore := setArgonCapacity(1)
	defer restore()
	acquireArgonSlot()
	defer releaseArgonSlot()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	ok, available := VerifySecretContext(ctx, "the right password", encoded)
	if available || ok {
		t.Fatal("a verification that timed out waiting for a slot must report unavailable")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancelled verification took %v; it should give up with the caller", elapsed)
	}
}

// TestVerifyEncodedContextSeparatesCancellationFromMismatch proves a cancelled
// caller is distinguishable from a wrong secret, so callers never count an
// abandoned attempt as a failed password (which would lock an innocent user
// out).
func TestVerifyEncodedContextSeparatesCancellationFromMismatch(t *testing.T) {
	// Deliberately not t.Parallel: see the note above about the shared gate.
	encoded, err := HashSecret("the right password")
	if err != nil {
		t.Fatal(err)
	}
	// A bcrypt row never touches the Argon gate, so it always answers.
	bcryptEncoded, err := BcryptHasher{Cost: 4}.Hash("token")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, available := VerifyEncodedContext(ctx, "token", bcryptEncoded); !ok || !available {
		t.Fatalf("bcrypt verify: ok=%v available=%v, want both true", ok, available)
	}
	if ok, _ := VerifyEncodedContext(context.Background(), "wrong", encoded); ok {
		t.Fatal("a wrong secret must not verify")
	}
	// A cancelled caller must never be *wrongly rejected*: if a free slot lets
	// the check run anyway, it must still return the true answer for a correct
	// secret. What it must never do is block with no capacity (proved above) or
	// report a mismatch it did not establish.
	if ok, available := VerifyEncodedContext(ctx, "the right password", encoded); available && !ok {
		t.Fatal("a verification that ran must not report a correct secret as a mismatch")
	}
	// With no capacity, a cancelled caller is told so instead of being parked.
	ctxTimeout, cancelTimeout := context.WithCancel(context.Background())
	cancelTimeout()
	restore := setArgonCapacity(1)
	acquireArgonSlot()
	ok, available := VerifyEncodedContext(ctxTimeout, "the right password", encoded)
	releaseArgonSlot()
	restore()
	if ok || available {
		t.Fatalf("cancelled argon verify with a full gate: ok=%v available=%v, want both false", ok, available)
	}
}
