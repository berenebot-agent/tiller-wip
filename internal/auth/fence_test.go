package auth

import (
	"sync/atomic"
	"testing"
)

// TestClientKeyFenceIsPerKey proves a mutation fences only the key it touched
// (and, for an account-wide revoke, that account). A verification for another
// key must never fail closed because someone else rotated theirs.
func TestClientKeyFenceIsPerKey(t *testing.T) {
	a := &ClientAuthenticator{mutated: map[string]mutationMark{}, accountMutated: map[string]mutationMark{}}
	start := atomic.LoadUint64(&a.seq)

	a.mu.Lock()
	a.markLocked("key-a", "")
	a.mu.Unlock()

	if !a.mutatedSinceLocked("key-a", "acct-a", start) {
		t.Error("the mutated key must fail closed")
	}
	if a.mutatedSinceLocked("key-b", "acct-b", start) {
		t.Error("an unrelated tenant's key must not fail closed")
	}
	if a.mutatedSinceLocked("key-a", "acct-a", atomic.LoadUint64(&a.seq)) {
		t.Error("a verification begun after the mutation must not fail closed")
	}

	// An account-wide revoke fences that account's keys, including ones not
	// individually marked, but no other account.
	accountStart := atomic.LoadUint64(&a.seq)
	a.mu.Lock()
	a.markLocked("", "acct-a")
	a.mu.Unlock()
	if !a.mutatedSinceLocked("key-c", "acct-a", accountStart) {
		t.Error("an account-wide revoke must fence that account's keys")
	}
	if a.mutatedSinceLocked("key-c", "acct-b", accountStart) {
		t.Error("an account-wide revoke must not fence another account")
	}
}

// TestManagementTokenFenceIsPerToken is the same invariant for the management
// token cache, which has its own authenticator.
func TestManagementTokenFenceIsPerToken(t *testing.T) {
	a := &ManagementTokenAuthenticator{mutated: map[string]mutationMark{}, accountMutated: map[string]mutationMark{}}
	start := atomic.LoadUint64(&a.seq)

	a.mu.Lock()
	a.invalidateLocked("token-a")
	a.mu.Unlock()

	if !a.mutatedSinceLocked("token-a", "acct-a", start) {
		t.Error("the mutated token must fail closed")
	}
	if a.mutatedSinceLocked("token-b", "acct-b", start) {
		t.Error("an unrelated tenant's token must not fail closed")
	}
}

// TestSessionFenceIsPerSelector proves logging out one admin session does not
// fail a concurrent load of another selector closed, while a global invalidation
// still fences every session.
func TestSessionFenceIsPerSelector(t *testing.T) {
	s := &SessionStore{revoked: map[string]mutationMark{}}
	start := atomic.LoadUint64(&s.seq)

	s.mu.Lock()
	s.markRevokedLocked("selector-a")
	s.mu.Unlock()

	if !s.invalidatedLocked("selector-a", start) {
		t.Error("the revoked selector must fail closed")
	}
	if s.invalidatedLocked("selector-b", start) {
		t.Error("another selector must not fail closed after a logout")
	}

	floorStart := atomic.LoadUint64(&s.seq)
	s.mu.Lock()
	atomic.StoreUint64(&s.invalidateAllAt, atomic.AddUint64(&s.seq, 1))
	s.mu.Unlock()
	if !s.invalidatedLocked("selector-b", floorStart) {
		t.Error("a global invalidation must fence every session")
	}
}
