package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	argonMemory      = 64 * 1024
	argonIterations  = 3
	argonParallelism = 4
	argonSaltBytes   = 16
	argonKeyBytes    = 32
)

type GeneratedKey struct {
	Plaintext   string
	Selector    string
	Hash        string
	Fingerprint string
}

func GenerateKey() (GeneratedKey, error) {
	selectorRaw, err := randomBytes(9)
	if err != nil {
		return GeneratedKey{}, err
	}
	secretRaw, err := randomBytes(32)
	if err != nil {
		return GeneratedKey{}, err
	}
	selector := base64.RawURLEncoding.EncodeToString(selectorRaw)
	secret := base64.RawURLEncoding.EncodeToString(secretRaw)
	phc, err := HashSecret(secret)
	if err != nil {
		return GeneratedKey{}, err
	}
	return GeneratedKey{
		Plaintext:   "sk-tr-" + selector + "." + secret,
		Selector:    selector,
		Hash:        phc,
		Fingerprint: secret[len(secret)-8:],
	}, nil
}

func HashSecret(secret string) (string, error) {
	salt, err := randomBytes(argonSaltBytes)
	if err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(secret), salt, argonIterations, argonMemory, argonParallelism, argonKeyBytes)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func VerifySecret(secret, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false
	}
	if memory != argonMemory || iterations != argonIterations || parallelism != argonParallelism {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != argonSaltBytes {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) != argonKeyBytes {
		return false
	}
	got := argon2.IDKey([]byte(secret), salt, iterations, memory, parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func ParseKey(key string) (selector, secret string, ok bool) {
	if !strings.HasPrefix(key, "sk-tr-") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(key, "sk-tr-"), ".")
	if len(parts) != 2 || len(parts[0]) != 12 || len(parts[1]) != 43 {
		return "", "", false
	}
	if _, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil {
		return "", "", false
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(parts[1]); err != nil || len(decoded) != 32 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

type ClientIdentity struct {
	ID      string
	Name    string
	Enabled bool
}

type cacheEntry struct {
	identity ClientIdentity
	expires  time.Time
}

type ClientAuthenticator struct {
	db      *sql.DB
	key     []byte
	ttl     time.Duration
	mu      sync.Mutex
	entries map[[32]byte]cacheEntry
}

func NewClientAuthenticator(db *sql.DB) (*ClientAuthenticator, error) {
	key, err := randomBytes(32)
	if err != nil {
		return nil, err
	}
	return &ClientAuthenticator{db: db, key: key, ttl: 30 * time.Second, entries: make(map[[32]byte]cacheEntry)}, nil
}

func (a *ClientAuthenticator) Authenticate(raw string) (ClientIdentity, bool) {
	selector, secret, ok := ParseKey(raw)
	if !ok {
		return ClientIdentity{}, false
	}
	cacheKey := a.cacheKey(raw)
	now := time.Now()
	a.mu.Lock()
	entry, found := a.entries[cacheKey]
	if found && now.Before(entry.expires) {
		a.mu.Unlock()
		return entry.identity, entry.identity.Enabled
	}
	if found {
		delete(a.entries, cacheKey)
	}
	a.mu.Unlock()

	var identity ClientIdentity
	var hash string
	err := a.db.QueryRow(`SELECT id,name,enabled,secret_hash FROM client_keys WHERE selector=?`, selector).
		Scan(&identity.ID, &identity.Name, &identity.Enabled, &hash)
	if err != nil || !identity.Enabled || !VerifySecret(secret, hash) {
		return ClientIdentity{}, false
	}
	a.mu.Lock()
	a.entries[cacheKey] = cacheEntry{identity: identity, expires: now.Add(a.ttl)}
	a.mu.Unlock()
	return identity, true
}

func (a *ClientAuthenticator) Invalidate(clientID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, entry := range a.entries {
		if entry.identity.ID == clientID {
			delete(a.entries, key)
		}
	}
}

func (a *ClientAuthenticator) cacheKey(raw string) [32]byte {
	h := hmac.New(sha256.New, a.key)
	_, _ = h.Write([]byte(raw))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return b, err
}

type Session struct {
	Token     string
	CSRFToken string
	ExpiresAt time.Time
}

type SessionStore struct {
	mu       sync.Mutex
	sessions map[[32]byte]Session
	key      []byte
	ttl      time.Duration
}

func NewSessionStore() (*SessionStore, error) {
	key, err := randomBytes(32)
	if err != nil {
		return nil, err
	}
	return &SessionStore{sessions: make(map[[32]byte]Session), key: key, ttl: 12 * time.Hour}, nil
}

func (s *SessionStore) Create() (Session, error) {
	token, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	session := Session{Token: token, CSRFToken: csrf, ExpiresAt: time.Now().Add(s.ttl)}
	s.mu.Lock()
	s.pruneLocked()
	s.sessions[s.digest(token)] = session
	s.mu.Unlock()
	return session, nil
}

func (s *SessionStore) Get(token string) (Session, bool) {
	if token == "" {
		return Session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.digest(token)
	session, ok := s.sessions[key]
	if !ok || time.Now().After(session.ExpiresAt) {
		if ok {
			delete(s.sessions, key)
		}
		return Session{}, false
	}
	return session, true
}

func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	delete(s.sessions, s.digest(token))
	s.mu.Unlock()
}

func (s *SessionStore) CheckCSRF(session Session, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(session.CSRFToken), []byte(token)) == 1
}

func (s *SessionStore) digest(token string) [32]byte {
	h := hmac.New(sha256.New, s.key)
	_, _ = h.Write([]byte(token))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func (s *SessionStore) pruneLocked() {
	now := time.Now()
	for key, session := range s.sessions {
		if now.After(session.ExpiresAt) {
			delete(s.sessions, key)
		}
	}
}

func randomToken(n int) (string, error) {
	b, err := randomBytes(n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func EqualCredential(got, want string) bool {
	gotHash := sha256.Sum256([]byte(got))
	wantHash := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
}

var ErrMalformedPHC = errors.New("malformed argon2id hash")

func ArgonParameters(encoded string) (memory uint32, iterations uint32, lanes uint8, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return 0, 0, 0, ErrMalformedPHC
	}
	values := strings.Split(parts[3], ",")
	if len(values) != 3 {
		return 0, 0, 0, ErrMalformedPHC
	}
	m, e1 := strconv.ParseUint(strings.TrimPrefix(values[0], "m="), 10, 32)
	t, e2 := strconv.ParseUint(strings.TrimPrefix(values[1], "t="), 10, 32)
	p, e3 := strconv.ParseUint(strings.TrimPrefix(values[2], "p="), 10, 8)
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, 0, 0, ErrMalformedPHC
	}
	return uint32(m), uint32(t), uint8(p), nil
}
