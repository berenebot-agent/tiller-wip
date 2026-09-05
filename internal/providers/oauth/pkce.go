package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const flowLifetime = 10 * time.Minute

var (
	ErrFlowActive  = errors.New("oauth connection already in progress")
	ErrFlowExpired = errors.New("oauth connection expired")
	ErrFlowInvalid = errors.New("oauth connection state is invalid")
	ErrCallback    = errors.New("oauth callback is invalid")
)

type PKCE struct {
	State     string
	Verifier  string
	Challenge string
}

func NewPKCE() (PKCE, error) {
	state, err := randomURLValue(32)
	if err != nil {
		return PKCE{}, err
	}
	verifier, err := randomURLValue(32)
	if err != nil {
		return PKCE{}, err
	}
	hash := sha256.Sum256([]byte(verifier))
	return PKCE{State: state, Verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(hash[:])}, nil
}

func randomURLValue(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type Callback struct {
	Code  string
	State string
}

// ParseCallback accepts only a complete redirected URL. Requiring state here
// keeps callers from accidentally treating an authorization state as a code.
func ParseCallback(raw string) (Callback, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() || u.Host == "" || u.Fragment != "" || u.Scheme != "http" && u.Scheme != "https" {
		return Callback{}, ErrCallback
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return Callback{}, ErrCallback
	}
	code := strings.TrimSpace(query.Get("code"))
	state := strings.TrimSpace(query.Get("state"))
	if code == "" || state == "" {
		return Callback{}, ErrCallback
	}
	return Callback{Code: code, State: state}, nil
}

type Flow struct {
	ProviderID string
	PKCE       PKCE
	CreatedAt  time.Time
}

type FlowStore struct {
	mu         sync.Mutex
	now        func() time.Time
	byProvider map[string]Flow
	path       string
}

func NewFlowStore(now func() time.Time) *FlowStore {
	return NewPersistentFlowStore(now, "")
}

func NewPersistentFlowStore(now func() time.Time, path string) *FlowStore {
	if now == nil {
		now = time.Now
	}
	store := &FlowStore{now: now, byProvider: make(map[string]Flow), path: path}
	store.load()
	return store
}

func (s *FlowStore) load() {
	if s.path == "" {
		return
	}
	body, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(body, &s.byProvider)
}

func (s *FlowStore) save() {
	if s.path == "" {
		return
	}
	body, err := json.Marshal(s.byProvider)
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err == nil {
		_ = os.Rename(tmp, s.path)
	}
}

func (s *FlowStore) Begin(providerID string) (Flow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	if flow, ok := s.byProvider[providerID]; ok && now.Sub(flow.CreatedAt) < flowLifetime {
		return Flow{}, ErrFlowActive
	}
	pkce, err := NewPKCE()
	if err != nil {
		return Flow{}, err
	}
	flow := Flow{ProviderID: providerID, PKCE: pkce, CreatedAt: now}
	s.byProvider[providerID] = flow
	s.save()
	return flow, nil
}

// Consume validates and removes a flow before the token exchange. This makes
// callbacks single-use even when the provider exchange fails.
func (s *FlowStore) Consume(providerID, state string) (Flow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	flow, ok := s.byProvider[providerID]
	if !ok && s.path != "" {
		s.load()
		flow, ok = s.byProvider[providerID]
	}
	if !ok {
		return Flow{}, ErrFlowInvalid
	}
	delete(s.byProvider, providerID)
	s.save()
	if s.now().UTC().Sub(flow.CreatedAt) >= flowLifetime {
		return Flow{}, ErrFlowExpired
	}
	if state == "" || state != flow.PKCE.State {
		return Flow{}, ErrFlowInvalid
	}
	return flow, nil
}

func (s *FlowStore) Cancel(providerID string) {
	s.mu.Lock()
	delete(s.byProvider, providerID)
	s.save()
	s.mu.Unlock()
}
