package identity

import (
	"sync/atomic"
	"testing"
)

// TestUserSessionFenceIsPerPrincipal proves a logout fences only its own
// selector: another user's concurrent session load must not fail closed. A
// whole-user invalidation fences that user (except an explicitly kept selector),
// and an account invalidation fences that account only.
func TestUserSessionFenceIsPerPrincipal(t *testing.T) {
	s := &Store{
		revoked:        map[string]mutationMark{},
		userMutated:    map[string]userMutation{},
		accountMutated: map[string]userMutation{},
	}
	start := atomic.LoadUint64(&s.seq)

	s.mu.Lock()
	s.markRevokedLocked("selector-a")
	s.mu.Unlock()

	if !s.invalidatedLocked("selector-a", "user-a", "acct-a", start) {
		t.Error("the revoked selector must fail closed")
	}
	if s.invalidatedLocked("selector-b", "user-b", "acct-b", start) {
		t.Error("another user's session must not fail closed after a logout")
	}

	// A whole-user invalidation (password/email change) fences that user's
	// sessions but keeps the selector the caller retained.
	userStart := atomic.LoadUint64(&s.seq)
	s.mu.Lock()
	s.userMutated["user-a"] = userMutation{seq: atomic.AddUint64(&s.seq, 1), keep: "selector-keep"}
	s.mu.Unlock()
	if !s.invalidatedLocked("selector-old", "user-a", "acct-a", userStart) {
		t.Error("a whole-user invalidation must fence that user's other sessions")
	}
	if s.invalidatedLocked("selector-keep", "user-a", "acct-a", userStart) {
		t.Error("the kept selector must survive a whole-user invalidation")
	}
	if s.invalidatedLocked("selector-b", "user-b", "acct-b", userStart) {
		t.Error("a whole-user invalidation must not fence another user")
	}

	// An account-wide invalidation (suspension/deletion) fences every user of
	// that account.
	accountStart := atomic.LoadUint64(&s.seq)
	s.mu.Lock()
	s.accountMutated["acct-a"] = userMutation{seq: atomic.AddUint64(&s.seq, 1)}
	s.mu.Unlock()
	if !s.invalidatedLocked("selector-old", "user-z", "acct-a", accountStart) {
		t.Error("an account invalidation must fence that account's sessions")
	}
	if s.invalidatedLocked("selector-b", "user-b", "acct-b", accountStart) {
		t.Error("an account invalidation must not fence another account")
	}
}

// TestPlatformSessionFenceIsPerSelector covers the platform console's own cache,
// which had the same global-counter pattern.
func TestPlatformSessionFenceIsPerSelector(t *testing.T) {
	s := &Store{platformRevoked: map[string]mutationMark{}}
	start := atomic.LoadUint64(&s.platformSeq)
	s.mu.Lock()
	s.platformRevoked["selector-a"] = mutationMark{seq: atomic.AddUint64(&s.platformSeq, 1)}
	s.mu.Unlock()

	if !s.platformInvalidatedLocked("selector-a", start) {
		t.Error("the revoked platform selector must fail closed")
	}
	if s.platformInvalidatedLocked("selector-b", start) {
		t.Error("another platform tab must not fail closed after a logout")
	}
}
