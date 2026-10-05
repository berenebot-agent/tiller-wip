package server

import (
	"crypto/sha256"
	"time"
)

// passkeyReauthTTL mirrors googleReauthTTL: short enough that a stale grant is
// useless, long enough to span the confirm dialog on the account page.
const passkeyReauthTTL = 5 * time.Minute

// passkeyReauthKey scopes a passkey grant to one session token. The purpose
// tag keeps it from ever being confused with a Google grant even though the
// stores are separate.
func passkeyReauthKey(sessionToken string) [32]byte {
	return sha256.Sum256([]byte("passkey\x00" + sessionToken))
}

// grantPasskeyReauth records that this session just completed a passkey
// assertion. The grant is single-use: a successful sensitive operation spends
// it via consumePasskeyReauth.
func (s *Server) grantPasskeyReauth(sessionToken string) {
	if sessionToken == "" {
		return
	}
	key := passkeyReauthKey(sessionToken)
	now := time.Now()
	s.passkeyReauthMu.Lock()
	defer s.passkeyReauthMu.Unlock()
	for existing, expires := range s.passkeyReauth {
		if !now.Before(expires) {
			delete(s.passkeyReauth, existing)
		}
	}
	if len(s.passkeyReauth) >= 100000 {
		for existing := range s.passkeyReauth {
			delete(s.passkeyReauth, existing)
			break
		}
	}
	s.passkeyReauth[key] = now.Add(passkeyReauthTTL)
}

// consumePasskeyReauth spends the grant for a session, returning whether one
// was valid. It is single-use so one confirmation cannot authorise several
// sensitive operations.
func (s *Server) consumePasskeyReauth(sessionToken string) bool {
	if sessionToken == "" {
		return false
	}
	key := passkeyReauthKey(sessionToken)
	now := time.Now()
	s.passkeyReauthMu.Lock()
	defer s.passkeyReauthMu.Unlock()
	expires, ok := s.passkeyReauth[key]
	delete(s.passkeyReauth, key)
	return ok && now.Before(expires)
}
