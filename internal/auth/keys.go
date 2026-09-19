package auth

import (
	"context"
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
	"sync/atomic"
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

// GenerateKeyWithHasher generates a client key using the provided hasher.
// The plaintext format, selector/secret lengths, fingerprint, and DB
// representation are unchanged regardless of hasher.
func GenerateKeyWithHasher(hasher SecretHasher) (GeneratedKey, error) {
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
	phc, err := hasher.Hash(secret)
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

// GenerateKey generates a client key using the production token hasher
// (bcrypt; client keys are high-entropy machine tokens).
func GenerateKey() (GeneratedKey, error) {
	return GenerateKeyWithHasher(BcryptHasher{})
}

func HashSecret(secret string) (string, error) {
	return argon2idHash(secret)
}

func VerifySecret(secret, encoded string) bool {
	return argon2idVerify(secret, encoded)
}

// argon2idHash is the canonical Argon2id implementation. Both Argon2Hasher
// and the package-level HashSecret delegate here.
func argon2idHash(secret string) (string, error) {
	salt, err := randomBytes(argonSaltBytes)
	if err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(secret), salt, argonIterations, argonMemory, argonParallelism, argonKeyBytes)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

// argon2idVerify is the canonical Argon2id verification. Both Argon2Hasher
// and the package-level VerifySecret delegate here.
func argon2idVerify(secret, encoded string) bool {
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
	ID        string
	AccountID string
	Name      string
	Enabled   bool
}

type cacheEntry struct {
	identity ClientIdentity
	expires  time.Time
}

// Auth cache defaults and bounds. Entries renew on use, so a longer TTL mainly
// reduces hash-verification work; a mutation-driven Invalidate is independent
// of the TTL. maxAuthCacheEntries is a safety valve against unbounded growth
// under heavy key churn (evicted keys simply re-verify on their next request).
const (
	defaultClientKeyCacheTTL = 15 * time.Minute
	defaultSessionCacheTTL   = 5 * time.Minute
	authCacheSweepInterval   = time.Minute
	maxAuthCacheEntries      = 100000
)

type ClientAuthenticator struct {
	db      *sql.DB
	key     []byte
	ttl     time.Duration
	hasher  SecretHasher
	mu      sync.Mutex
	entries map[[32]byte]cacheEntry
	// maxEntries caps the cache; a test may lower it.
	maxEntries int
	sem        chan struct{}
	rev        uint64
}

// NewClientAuthenticatorWithHasher constructs a ClientAuthenticator with the
// given hasher for client-key verification.
func NewClientAuthenticatorWithHasher(db *sql.DB, hasher SecretHasher) (*ClientAuthenticator, error) {
	key, err := randomBytes(32)
	if err != nil {
		return nil, err
	}
	if hasher == nil {
		hasher = BcryptHasher{}
	}
	return &ClientAuthenticator{db: db, key: key, ttl: defaultClientKeyCacheTTL, hasher: hasher, entries: make(map[[32]byte]cacheEntry), maxEntries: maxAuthCacheEntries, sem: make(chan struct{}, 4)}, nil
}

// SetCacheTTL overrides the verified-key cache TTL. It is called once at
// construction time, before the authenticator serves requests.
func (a *ClientAuthenticator) SetCacheTTL(d time.Duration) {
	if d <= 0 {
		return
	}
	a.mu.Lock()
	a.ttl = d
	a.mu.Unlock()
}

// NewClientAuthenticator constructs a ClientAuthenticator using the production
// token hasher (bcrypt; client keys are high-entropy machine tokens). It is the
// production wrapper.
func NewClientAuthenticator(db *sql.DB) (*ClientAuthenticator, error) {
	return NewClientAuthenticatorWithHasher(db, BcryptHasher{})
}

func (a *ClientAuthenticator) Authenticate(raw string) (ClientIdentity, bool) {
	identity, ok, _ := a.AuthenticateContext(context.Background(), raw)
	return identity, ok
}

func (a *ClientAuthenticator) AuthenticateContext(ctx context.Context, raw string) (ClientIdentity, bool, bool) {
	selector, secret, ok := ParseKey(raw)
	if !ok {
		return ClientIdentity{}, false, true
	}
	cacheKey := a.cacheKey(raw)
	now := time.Now()
	a.mu.Lock()
	entry, found := a.entries[cacheKey]
	if found && now.Before(entry.expires) {
		// Sliding expiry: an actively-used key stays warm and verifies once
		// rather than once per TTL window.
		entry.expires = now.Add(a.ttl)
		a.entries[cacheKey] = entry
		a.mu.Unlock()
		return entry.identity, entry.identity.Enabled, true
	}
	if found {
		delete(a.entries, cacheKey)
	}
	a.mu.Unlock()

	select {
	case a.sem <- struct{}{}:
	case <-ctx.Done():
		return ClientIdentity{}, false, false
	default:
		return ClientIdentity{}, false, false
	}
	defer func() { <-a.sem }()

	generation := atomic.LoadUint64(&a.rev)
	var identity ClientIdentity
	var hash string
	err := a.db.QueryRowContext(ctx, `SELECT id,account_id,name,enabled,secret_hash FROM client_keys WHERE selector=?`, selector).
		Scan(&identity.ID, &identity.AccountID, &identity.Name, &identity.Enabled, &hash)
	if err != nil || !identity.Enabled || !a.hasher.Verify(secret, hash) {
		return ClientIdentity{}, false, true
	}
	// Lazy migration: a successful verify of a legacy argon2id key upgrades the
	// stored hash to the current algorithm, best-effort and outside the cache
	// lock. The compare-and-swap on oldHash makes this safe against a concurrent
	// rotation (the update becomes a no-op if secret_hash already changed).
	if a.hasher.NeedsRehash(hash) {
		if upgraded, herr := a.hasher.Hash(secret); herr == nil {
			_, _ = a.db.ExecContext(ctx, `UPDATE client_keys SET secret_hash=?, updated_at=? WHERE id=? AND secret_hash=?`,
				upgraded, formatUTC(time.Now()), identity.ID, hash)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if atomic.LoadUint64(&a.rev) != generation {
		return ClientIdentity{}, false, true
	}
	a.ensureCacheRoomLocked(now)
	a.entries[cacheKey] = cacheEntry{identity: identity, expires: now.Add(a.ttl)}
	return identity, true, true
}

// ensureCacheRoomLocked makes room for a new cache entry: it first drops
// expired entries, then — only if still at the cap — evicts arbitrary entries.
// Called with a.mu held.
func (a *ClientAuthenticator) ensureCacheRoomLocked(now time.Time) {
	if len(a.entries) < a.maxEntries {
		return
	}
	a.sweepExpiredLocked(now)
	for len(a.entries) >= a.maxEntries {
		for key := range a.entries {
			delete(a.entries, key)
			break
		}
	}
}

// SweepExpired removes expired entries from the verified-key cache.
func (a *ClientAuthenticator) SweepExpired() {
	a.mu.Lock()
	a.sweepExpiredLocked(time.Now())
	a.mu.Unlock()
}

func (a *ClientAuthenticator) sweepExpiredLocked(now time.Time) {
	for key, entry := range a.entries {
		if !now.Before(entry.expires) {
			delete(a.entries, key)
		}
	}
}

// StartSweeper periodically evicts expired cache entries until ctx is done.
func (a *ClientAuthenticator) StartSweeper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(authCacheSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.SweepExpired()
			}
		}
	}()
}

func (a *ClientAuthenticator) Invalidate(clientID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.invalidateLocked(clientID)
}

// InvalidateWith publishes a client mutation while cache publication is
// excluded, so verification begun against the old database state cannot
// authorize after the mutation commits.
func (a *ClientAuthenticator) InvalidateWith(clientID string, publish func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := publish(); err != nil {
		return err
	}
	a.invalidateLocked(clientID)
	return nil
}

func (a *ClientAuthenticator) invalidateLocked(clientID string) {
	atomic.AddUint64(&a.rev, 1)
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
	credentialHashKey    = "admin_credential_hash"
)

type sessionCacheEntry struct {
	session Session
	expires time.Time
	// secretHash binds the cached entry to the presented secret so a cache
	// hit still proves knowledge of the secret. Without this, anyone
	// presenting only the selector would hit the cache and skip verification.
	secretHash [32]byte
}

// SessionStore persists admin sessions in the database so they survive process
// and container restarts. The raw session secret is never stored; only a hash
// of it is persisted. A short-lived in-memory cache avoids recomputing the hash
// on every request. The hashers are injected so tests can use a fast
// implementation; production uses bcrypt for session tokens and argon2id for
// the admin credential fingerprint.
type SessionStore struct {
	db  *sql.DB
	ttl time.Duration
	// tokenHasher hashes session tokens (high-entropy, per-login).
	tokenHasher SecretHasher
	// credentialHasher hashes the admin credential fingerprint (low-entropy,
	// human-chosen), which keeps the memory-hard KDF.
	credentialHasher SecretHasher
	// cacheTTL bounds how long a validated session token is cached in memory.
	cacheTTL time.Duration
	// maxEntries caps the cache; a test may lower it.
	maxEntries int
	mu         sync.Mutex
	cache      map[string]sessionCacheEntry
	rev        uint64
}

// NewSessionStoreWithHasher constructs a SessionStore that uses the same hasher
// for session tokens and the admin credential fingerprint. It exists for tests,
// which inject a fast hasher; production uses NewSessionStore.
func NewSessionStoreWithHasher(db *sql.DB, username, password string, ttl time.Duration, hasher SecretHasher) (*SessionStore, error) {
	return NewSessionStoreTiered(db, username, password, ttl, hasher, hasher)
}

// NewSessionStoreTiered constructs a SessionStore with separate hashers for
// session tokens and the admin credential fingerprint. Either nil falls back to
// the production default for its role (bcrypt tokens, argon2id credential).
func NewSessionStoreTiered(db *sql.DB, username, password string, ttl time.Duration, tokenHasher, credentialHasher SecretHasher) (*SessionStore, error) {
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	if tokenHasher == nil {
		tokenHasher = BcryptHasher{}
	}
	if credentialHasher == nil {
		credentialHasher = Argon2Hasher{}
	}
	s := &SessionStore{db: db, ttl: ttl, tokenHasher: tokenHasher, credentialHasher: credentialHasher, cacheTTL: defaultSessionCacheTTL, maxEntries: maxAuthCacheEntries, cache: make(map[string]sessionCacheEntry)}
	if err := s.syncCredential(username, password); err != nil {
		return nil, err
	}
	return s, nil
}

// SetCacheTTL overrides the validated-session cache TTL. It is called once at
// construction time, before the store serves requests.
func (s *SessionStore) SetCacheTTL(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	s.cacheTTL = d
	s.mu.Unlock()
}

// NewSessionStore constructs a SessionStore using the production tiered hashers:
// bcrypt for high-entropy session tokens and argon2id for the low-entropy admin
// credential fingerprint.
func NewSessionStore(db *sql.DB, username, password string, ttl time.Duration) (*SessionStore, error) {
	return NewSessionStoreTiered(db, username, password, ttl, BcryptHasher{}, Argon2Hasher{})
}

// syncCredential stores a fingerprint of the admin credentials and invalidates
// all existing sessions if the credentials changed since the last start, so a
// material username/password change forces a fresh login.
func (s *SessionStore) syncCredential(username, password string) error {
	material := username + "\x00" + password
	var stored string
	err := s.db.QueryRow(`SELECT value FROM platform_settings WHERE key=?`, credentialHashKey).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		hash, err := s.credentialHasher.Hash(material)
		if err != nil {
			return err
		}
		_, err = s.db.Exec(`INSERT INTO platform_settings(key,value,updated_at) VALUES(?,?,?)`, credentialHashKey, hash, formatUTC(time.Now()))
		return err
	}
	if err != nil {
		return err
	}
	if s.credentialHasher.Verify(material, stored) {
		return nil
	}
	if err := s.InvalidateAll(); err != nil {
		return err
	}
	hash, err := s.credentialHasher.Hash(material)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE platform_settings SET value=?, updated_at=? WHERE key=?`, hash, formatUTC(time.Now()), credentialHashKey)
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
	hash, err := s.tokenHasher.Hash(sec)
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
			want := sha256.Sum256([]byte(secret))
			if subtle.ConstantTimeCompare(want[:], entry.secretHash[:]) == 1 {
				// Sliding expiry: an actively-used session stays warm.
				entry.expires = now.Add(s.cacheTTL)
				s.cache[selector] = entry
				s.mu.Unlock()
				return entry.session, true
			}
		}
		delete(s.cache, selector)
	}
	s.mu.Unlock()
	generation := atomic.LoadUint64(&s.rev)

	var csrfToken, tokenHash, expiresAt string
	err := s.db.QueryRow(`SELECT csrf_token, token_hash, expires_at FROM admin_sessions WHERE id=?`, selector).Scan(&csrfToken, &tokenHash, &expiresAt)
	if err != nil {
		return Session{}, false
	}
	exp, perr := time.Parse(time.RFC3339Nano, expiresAt)
	if perr != nil || now.After(exp) {
		s.revoke(selector)
		return Session{}, false
	}
	if !s.tokenHasher.Verify(secret, tokenHash) {
		return Session{}, false
	}
	// Lazy migration: upgrade a legacy argon2id session token hash after a
	// successful verify. Best-effort; sessions also upgrade on next login.
	s.rehashSessionToken(selector, secret, tokenHash)
	// Sliding expiry: extend when more than half the lifetime has elapsed.
	if now.Add(s.ttl / 2).After(exp) {
		exp = now.Add(s.ttl)
		_, _ = s.db.Exec(`UPDATE admin_sessions SET expires_at=?, last_used_at=? WHERE id=?`, formatUTC(exp), formatUTC(now), selector)
	}
	session := Session{CSRFToken: csrfToken, ExpiresAt: exp}
	s.mu.Lock()
	defer s.mu.Unlock()
	if atomic.LoadUint64(&s.rev) != generation {
		return Session{}, false
	}
	s.ensureCacheRoomLocked(now)
	s.cache[selector] = sessionCacheEntry{session: session, expires: now.Add(s.cacheTTL), secretHash: sha256.Sum256([]byte(secret))}
	return session, true
}

// ensureCacheRoomLocked makes room for a new session cache entry: drop expired
// entries first, then evict arbitrary entries only if still at the cap. Called
// with s.mu held.
func (s *SessionStore) ensureCacheRoomLocked(now time.Time) {
	if len(s.cache) < s.maxEntries {
		return
	}
	s.sweepExpiredLocked(now)
	for len(s.cache) >= s.maxEntries {
		for key := range s.cache {
			delete(s.cache, key)
			break
		}
	}
}

// SweepExpired removes expired entries from the session cache.
func (s *SessionStore) SweepExpired() {
	s.mu.Lock()
	s.sweepExpiredLocked(time.Now())
	s.mu.Unlock()
}

func (s *SessionStore) sweepExpiredLocked(now time.Time) {
	for key, entry := range s.cache {
		if !now.Before(entry.expires) {
			delete(s.cache, key)
		}
	}
}

// StartSweeper periodically evicts expired session cache entries until ctx is
// done.
func (s *SessionStore) StartSweeper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(authCacheSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.SweepExpired()
			}
		}
	}()
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
	if err != nil || !time.Now().Before(exp) || !s.tokenHasher.Verify(secret, tokenHash) {
		return Session{}, false
	}
	s.rehashSessionToken(selector, secret, tokenHash)
	return Session{CSRFToken: csrfToken, ExpiresAt: exp}, true
}

// rehashSessionToken upgrades a session's stored token hash to the store's
// current token algorithm after a successful verify. The compare-and-swap on
// oldHash makes it a no-op if the row already changed. Best-effort: a failure
// only means the token is upgraded on a later verify (or on next login).
func (s *SessionStore) rehashSessionToken(selector, secret, oldHash string) {
	if !s.tokenHasher.NeedsRehash(oldHash) {
		return
	}
	upgraded, err := s.tokenHasher.Hash(secret)
	if err != nil {
		return
	}
	_, _ = s.db.Exec(`UPDATE admin_sessions SET token_hash=? WHERE id=? AND token_hash=?`, upgraded, selector, oldHash)
}

func (s *SessionStore) Delete(token string) {
	selector, _, ok := parseSessionToken(token)
	if !ok {
		return
	}
	s.revoke(selector)
}

func (s *SessionStore) revoke(selector string) {
	s.mu.Lock()
	_, _ = s.db.Exec(`DELETE FROM admin_sessions WHERE id=?`, selector)
	atomic.AddUint64(&s.rev, 1)
	delete(s.cache, selector)
	s.mu.Unlock()
}

func (s *SessionStore) CheckCSRF(session Session, token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(session.CSRFToken), []byte(token)) == 1
}

// InvalidateAll revokes every admin session, e.g. after a credential change.
func (s *SessionStore) InvalidateAll() error {
	s.mu.Lock()
	_, err := s.db.Exec(`DELETE FROM admin_sessions`)
	atomic.AddUint64(&s.rev, 1)
	s.cache = make(map[string]sessionCacheEntry)
	s.mu.Unlock()
	return err
}

// ParseSessionToken splits a session token into its selector and secret
// components. Exported for use by tests in external packages.
func ParseSessionToken(token string) (selector, secret string, ok bool) {
	return parseSessionToken(token)
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
	h := hmac.New(sha256.New, credentialHMACKey)
	h.Write([]byte(got))
	gotMAC := h.Sum(nil)
	h.Reset()
	h.Write([]byte(want))
	wantMAC := h.Sum(nil)
	return subtle.ConstantTimeCompare(gotMAC, wantMAC) == 1
}

var credentialHMACKey = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("auth: " + err.Error())
	}
	return b
}()

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
