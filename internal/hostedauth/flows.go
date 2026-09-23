package hostedauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"
)

const FlowTTL = 10 * time.Minute

type Intent string

const (
	IntentSignIn Intent = "signin"
	IntentLink   Intent = "link"
	IntentReauth Intent = "reauth"
)

type Flow struct {
	State        string
	Nonce        string
	Verifier     string
	Intent       Intent
	SessionToken string
	ExpiresAt    time.Time
}

// FlowStore holds one-use OAuth state only in memory. A restart expires every
// flow, which is safer than persisting browser authentication state.
type FlowStore struct {
	mu    sync.Mutex
	flows map[string]Flow
}

func NewFlowStore() *FlowStore { return &FlowStore{flows: make(map[string]Flow)} }

func (s *FlowStore) Put(flow Flow) bool {
	if s == nil || flow.State == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, current := range s.flows {
		if !now.Before(current.ExpiresAt) {
			delete(s.flows, key)
		}
	}
	if len(s.flows) >= 4096 {
		return false
	}
	s.flows[flow.State] = flow
	return true
}

func (s *FlowStore) Take(state string) (Flow, bool) {
	if s == nil || state == "" {
		return Flow{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	flow, ok := s.flows[state]
	delete(s.flows, state)
	return flow, ok && time.Now().Before(flow.ExpiresAt)
}

type SignupClaims struct {
	Subject string
	Email   string
	Expires time.Time
}

// PendingSignupStore retains a validated Google identity until the visitor
// explicitly accepts the current legal documents. Cookie tokens are hashed in
// memory just as a precaution against accidental map inspection or dumps.
type PendingSignupStore struct {
	mu      sync.Mutex
	entries map[[32]byte]SignupClaims
}

func NewPendingSignupStore() *PendingSignupStore {
	return &PendingSignupStore{entries: make(map[[32]byte]SignupClaims)}
}

func (s *PendingSignupStore) Put(claims SignupClaims) (string, bool) {
	if s == nil || claims.Subject == "" || claims.Email == "" {
		return "", false
	}
	token, err := randomToken()
	if err != nil {
		return "", false
	}
	key := sha256.Sum256([]byte(token))
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for currentKey, current := range s.entries {
		if !now.Before(current.Expires) {
			delete(s.entries, currentKey)
		}
	}
	if len(s.entries) >= 4096 {
		return "", false
	}
	claims.Expires = now.Add(FlowTTL)
	s.entries[key] = claims
	return token, true
}

func (s *PendingSignupStore) Take(token string) (SignupClaims, bool) {
	if s == nil || token == "" {
		return SignupClaims{}, false
	}
	key := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	claims, ok := s.entries[key]
	delete(s.entries, key)
	return claims, ok && time.Now().Before(claims.Expires)
}

func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
