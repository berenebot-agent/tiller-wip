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
	memory, iterations, parallelism, err := ArgonParameters(encoded)
	if err != nil || memory != argonMemory || iterations != argonIterations || parallelism != argonParallelism {
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

const (
	sessionSelectorBytes = 16
	sessionSecretBytes   = 32
	sessionCacheTTL      = 5 * time.Minute
	credentialHashKey    = "admin_credential_hash"
)

type sessionCacheEntry struct {
	session Session
	expires time.Time
}

// SessionStore persists admin sessions in the database so they survive process
// and container restarts. The raw session secret is never stored; only an
// argon2id hash of it is persisted. A short-lived in-memory cache avoids
// recomputing the argon2id hash on every request.
type SessionStore struct {
	db    *sql.DB
	ttl   time.Duration
	mu    sync.Mutex
	cache map[string]sessionCacheEntry
}

func NewSessionStore(db *sql.DB, username, password string, ttl time.Duration) (*SessionStore, error) {
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	s := &SessionStore{db: db, ttl: ttl, cache: make(map[string]sessionCacheEntry)}
	if err := s.syncCredential(username, password); err != nil {
		return nil, err
	}
	return s, nil
}

// syncCredential stores an argon2id fingerprint of the admin credentials and
// invalidates all existing sessions if the credentials changed since the last
// start, so a material username/password change forces a fresh login.
func (s *SessionStore) syncCredential(username, password string) error {
	material := username + "\x00" + password
	var stored string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, credentialHashKey).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		hash, err := HashSecret(material)
		if err != nil {
			return err
		}
		_, err = s.db.Exec(`INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)`, credentialHashKey, hash, formatUTC(time.Now()))
		return err
	}
	if err != nil {
		return err
	}
	if VerifySecret(material, stored) {
		return nil
	}
	if err := s.InvalidateAll(); err != nil {
		return err
	}
	hash, err := HashSecret(material)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE settings SET value=?, updated_at=? WHERE key=?`, hash, formatUTC(time.Now()), credentialHashKey)
	return err
}

func (s *SessionStore) Create() (Session, error) {
	selector, err := randomBytes(sessionSelectorBytes)
	if err != nil {
		return Session{}, err
	}
	secret, err := randomBytes(sessionSecretBytes)
	if err != nil {
		return Session{}, err
	}
	csrf, err := randomBytes(32)
	if err != nil {
		return Session{}, err
	}
	sel := base64.RawURLEncoding.EncodeToString(selector)
	sec := base64.RawURLEncoding.EncodeToString(secret)
	hash, err := HashSecret(sec)
	if err != nil {
		return Session{}, err
	}
	now := time.Now()
	expires := now.Add(s.ttl)
	session := Session{Token: sel + "." + sec, CSRFToken: base64.RawURLEncoding.EncodeToString(csrf), ExpiresAt: expires}
	_, err = s.db.Exec(`INSERT INTO admin_sessions(id,token_hash,csrf_token,created_at,expires_at,last_used_at) VALUES(?,?,?,?,?,?)`,
		sel, hash, session.CSRFToken, formatUTC(now), formatUTC(expires), formatUTC(now))
	if err != nil {
		return Session{}, err
	}
	return session, nil
}

func (s *SessionStore) Get(token string) (Session, bool) {
	selector, secret, ok := parseSessionToken(token)
	if !ok {
		return Session{}, false
	}
	now := time.Now()
	s.mu.Lock()
	if entry, found := s.cache[selector]; found {
		if now.Before(entry.expires) && now.Before(entry.session.ExpiresAt) {
			s.mu.Unlock()
			return entry.session, true
		}
		delete(s.cache, selector)
	}
	s.mu.Unlock()

	var csrfToken, tokenHash, expiresAt string
	err := s.db.QueryRow(`SELECT csrf_token, token_hash, expires_at FROM admin_sessions WHERE id=?`, selector).Scan(&csrfToken, &tokenHash, &expiresAt)
	if err != nil {
		return Session{}, false
	}
	exp, perr := time.Parse(time.RFC3339Nano, expiresAt)
	if perr != nil || now.After(exp) || !VerifySecret(secret, tokenHash) {
		_, _ = s.db.Exec(`DELETE FROM admin_sessions WHERE id=?`, selector)
		return Session{}, false
	}
	// Sliding expiry: extend when more than half the lifetime has elapsed.
	if now.Add(s.ttl / 2).After(exp) {
		exp = now.Add(s.ttl)
		_, _ = s.db.Exec(`UPDATE admin_sessions SET expires_at=?, last_used_at=? WHERE id=?`, formatUTC(exp), formatUTC(now), selector)
	}
	session := Session{CSRFToken: csrfToken, ExpiresAt: exp}
	s.mu.Lock()
	s.cache[selector] = sessionCacheEntry{session: session, expires: now.Add(sessionCacheTTL)}
	s.mu.Unlock()
	return session, true
}

// Validate checks the persisted session without extending its sliding expiry.
// Long-lived requests use this so an open connection cannot keep a session
// alive indefinitely while still observing revocation.
func (s *SessionStore) Validate(token string) (Session, bool) {
	selector, secret, ok := parseSessionToken(token)
	if !ok {
		return Session{}, false
	}
	var csrfToken, tokenHash, expiresAt string
	if err := s.db.QueryRow(`SELECT csrf_token, token_hash, expires_at FROM admin_sessions WHERE id=?`, selector).Scan(&csrfToken, &tokenHash, &expiresAt); err != nil {
		return Session{}, false
	}
	exp, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !time.Now().Before(exp) || !VerifySecret(secret, tokenHash) {
		return Session{}, false
	}
	return Session{CSRFToken: csrfToken, ExpiresAt: exp}, true
}

func (s *SessionStore) Delete(token string) {
	selector, _, ok := parseSessionToken(token)
	if !ok {
		return
	}
	_, _ = s.db.Exec(`DELETE FROM admin_sessions WHERE id=?`, selector)
	s.mu.Lock()
	delete(s.cache, selector)
	s.mu.Unlock()
}

func (s *SessionStore) CheckCSRF(session Session, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(session.CSRFToken), []byte(token)) == 1
}

// InvalidateAll revokes every admin session, e.g. after a credential change.
func (s *SessionStore) InvalidateAll() error {
	_, err := s.db.Exec(`DELETE FROM admin_sessions`)
	s.mu.Lock()
	s.cache = make(map[string]sessionCacheEntry)
	s.mu.Unlock()
	return err
}

func parseSessionToken(token string) (selector, secret string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	if _, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil {
		return "", "", false
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(parts[1]); err != nil || len(decoded) != sessionSecretBytes {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func formatUTC(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

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
