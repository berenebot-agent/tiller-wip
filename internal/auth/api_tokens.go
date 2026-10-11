package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tiller-router/tiller-router/internal/store"
)

// ManagementTokenPrefix distinguishes management tokens (the admin REST
// surface) from client keys (`sk-tr-`, the inference surface). The two
// families share key geometry but never authenticates the other's routes.
const ManagementTokenPrefix = "mt-tr-"

// Scope tiers. A token's scope set is a per-area ceiling: a `write` grant
// satisfies a `read` requirement, a `delete` grant satisfies both.
const (
	ScopeRead   = 1
	ScopeWrite  = 2
	ScopeDelete = 3
)

// managementScopeAreas is the complete set of grantable resource areas. An
// unknown area in a request is rejected; a missing area means no access.
var managementScopeAreas = map[string]bool{
	"providers":   true,
	"virtual":     true,
	"client_keys": true,
	"settings":    true,
	"monitoring":  true,
}

// ManagementScopeAreas returns the grantable area names.
func ManagementScopeAreas() []string {
	return []string{"providers", "virtual", "client_keys", "settings", "monitoring"}
}

// ValidateManagementScopes validates a requested scope set and returns its
// canonical JSON representation for storage. Unknown areas and out-of-range
// tiers are rejected; an empty set is valid (the token can only introspect
// itself).
func ValidateManagementScopes(scopes map[string]int) (string, error) {
	canonical := make(map[string]int, len(scopes))
	for area, tier := range scopes {
		if !managementScopeAreas[area] {
			return "", fmt.Errorf("unknown scope area %q", area)
		}
		if tier < ScopeRead || tier > ScopeDelete {
			return "", fmt.Errorf("scope tier for %q must be between %d and %d", area, ScopeRead, ScopeDelete)
		}
		canonical[area] = tier
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// ParseManagementScopes decodes a stored scope set. A malformed value fails
// closed to an empty set (no access).
func ParseManagementScopes(raw string) map[string]int {
	if raw == "" {
		return nil
	}
	var scopes map[string]int
	if err := json.Unmarshal([]byte(raw), &scopes); err != nil {
		return nil
	}
	return scopes
}

// FullManagementScopes is the scope set the environment boot seed grants: every
// area at the delete ceiling (which implies write and read). It is the single
// super-user token type the embed contract exposes.
func FullManagementScopes() map[string]int {
	scopes := make(map[string]int, len(managementScopeAreas))
	for area := range managementScopeAreas {
		scopes[area] = ScopeDelete
	}
	return scopes
}

// SplitManagementToken splits a plaintext management token into its selector,
// secret, and hash/fingerprint components so the environment boot seed can
// store a hash of an operator-supplied value. The plaintext must already parse
// as a management token.
func SplitManagementToken(plaintext string, hasher SecretHasher) (selector, hash, fingerprint string, err error) {
	selector, secret, ok := ParseManagementToken(plaintext)
	if !ok {
		return "", "", "", fmt.Errorf("auth: not a valid management token")
	}
	phc, herr := hasher.Hash(secret)
	if herr != nil {
		return "", "", "", herr
	}
	return selector, phc, secret[len(secret)-8:], nil
}

// GenerateManagementTokenWithHasher generates a management token using the
// provided hasher. Secrets are high-entropy machine tokens, so production
// callers use the bcrypt tier (see the server wiring).
func GenerateManagementTokenWithHasher(hasher SecretHasher) (GeneratedKey, error) {
	return generateKeyWithPrefix(hasher, ManagementTokenPrefix)
}

// ParseManagementToken splits a management-token plaintext into selector and
// secret. Client keys are explicitly not accepted.
func ParseManagementToken(key string) (selector, secret string, ok bool) {
	return parseKeyWithPrefix(key, ManagementTokenPrefix)
}

// ManagementTokenIdentity is a verified management-token principal.
type ManagementTokenIdentity struct {
	ID            string
	AccountID     string
	AccountStatus string
	Name          string
	Enabled       bool
	Scopes        map[string]int
	// ExpiresAt is the token's expiry; the zero time means "never".
	ExpiresAt time.Time
}

// Allows reports whether the token's ceiling for area meets the required tier.
func (m ManagementTokenIdentity) Allows(area string, tier int) bool {
	return m.Scopes[area] >= tier
}

// Authentication outcomes for AuthenticateContext. All of them are terminal
// 401/429 outcomes; callers must not fall back to another credential source.
var (
	ErrManagementTokenInvalid = errors.New("auth: management token invalid")
	ErrManagementTokenExpired = errors.New("auth: management token expired")
	// ErrAuthUnavailable means verification capacity is saturated or the
	// request was cancelled before a slot opened; the caller may retry later.
	ErrAuthUnavailable = errors.New("auth: authentication temporarily unavailable")
)

type managementCacheEntry struct {
	identity ManagementTokenIdentity
	expires  time.Time
}

// ManagementTokenAuthenticator verifies management tokens against the
// api_tokens table, with a short-lived verified-token cache so a token does
// not pay a bcrypt verify on every request. It mirrors ClientAuthenticator's
// cache and invalidation semantics but is deliberately a separate type: the
// two credential families must never share a verification path.
type ManagementTokenAuthenticator struct {
	store      *store.Store
	key        []byte
	ttl        time.Duration
	hasher     SecretHasher
	mu         sync.Mutex
	entries    map[[32]byte]managementCacheEntry
	maxEntries int
	sem        chan struct{}
	rev        uint64
}

// NewManagementTokenAuthenticatorWithHasher constructs a
// ManagementTokenAuthenticator with the given hasher (tests inject a fast
// one; production uses the server's bcrypt-family token hasher).
func NewManagementTokenAuthenticatorWithHasher(db *sql.DB, hasher SecretHasher) (*ManagementTokenAuthenticator, error) {
	key, err := randomBytes(32)
	if err != nil {
		return nil, err
	}
	if hasher == nil {
		hasher = BcryptHasher{}
	}
	return &ManagementTokenAuthenticator{store: store.New(db), key: key, ttl: defaultClientKeyCacheTTL, hasher: hasher, entries: make(map[[32]byte]managementCacheEntry), maxEntries: maxAuthCacheEntries, sem: make(chan struct{}, 4)}, nil
}

// SetCacheTTL overrides the verified-token cache TTL. It is called once at
// construction time, before the authenticator serves requests.
func (a *ManagementTokenAuthenticator) SetCacheTTL(d time.Duration) {
	if d <= 0 {
		return
	}
	a.mu.Lock()
	a.ttl = d
	a.mu.Unlock()
}

// AuthenticateContext verifies a raw management token. On success it returns
// the token principal; every failure is one of the exported sentinel errors
// and must be treated as terminal (never a fallback to cookie authentication).
func (a *ManagementTokenAuthenticator) AuthenticateContext(ctx context.Context, raw string) (ManagementTokenIdentity, error) {
	selector, secret, ok := ParseManagementToken(raw)
	if !ok {
		return ManagementTokenIdentity{}, ErrManagementTokenInvalid
	}
	cacheKey := a.cacheKey(raw)
	now := time.Now()
	a.mu.Lock()
	if entry, found := a.entries[cacheKey]; found {
		if now.Before(entry.expires) {
			if !entry.identity.ExpiresAt.IsZero() && !now.Before(entry.identity.ExpiresAt) {
				delete(a.entries, cacheKey)
				a.mu.Unlock()
				return ManagementTokenIdentity{}, ErrManagementTokenExpired
			}
			// Sliding expiry: an actively-used token stays warm.
			entry.expires = now.Add(a.ttl)
			a.entries[cacheKey] = entry
			a.mu.Unlock()
			return entry.identity, nil
		}
		delete(a.entries, cacheKey)
	}
	a.mu.Unlock()

	select {
	case a.sem <- struct{}{}:
	case <-ctx.Done():
		return ManagementTokenIdentity{}, ErrAuthUnavailable
	default:
		return ManagementTokenIdentity{}, ErrAuthUnavailable
	}
	defer func() { <-a.sem }()

	generation := atomic.LoadUint64(&a.rev)
	row, err := a.store.ManagementTokenBySelector(ctx, selector)
	if err != nil || !row.Enabled || row.AccountStatus != "active" || !a.hasher.Verify(secret, row.SecretHash) {
		return ManagementTokenIdentity{}, ErrManagementTokenInvalid
	}
	identity := ManagementTokenIdentity{
		ID:            row.ID,
		AccountID:     row.AccountID,
		AccountStatus: row.AccountStatus,
		Name:          row.Name,
		Enabled:       row.Enabled,
		Scopes:        ParseManagementScopes(row.Scopes),
	}
	if row.ExpiresAt.Valid && row.ExpiresAt.String != "" {
		if exp, perr := time.Parse(time.RFC3339Nano, row.ExpiresAt.String); perr == nil {
			identity.ExpiresAt = exp
		}
	}
	// Lazy migration: a successful verify of a legacy argon2id key upgrades
	// the stored hash to the current algorithm, best-effort and outside the
	// cache lock.
	if a.hasher.NeedsRehash(row.SecretHash) {
		if upgraded, herr := a.hasher.Hash(secret); herr == nil {
			_ = a.store.UpdateManagementTokenSecretHash(ctx, identity.ID, row.SecretHash, upgraded, formatUTC(time.Now()))
		}
	}
	if !identity.ExpiresAt.IsZero() && !now.Before(identity.ExpiresAt) {
		return identity, ErrManagementTokenExpired
	}
	_ = a.store.TouchManagementTokenLastUsed(ctx, identity.ID, formatUTC(now))
	a.mu.Lock()
	defer a.mu.Unlock()
	if atomic.LoadUint64(&a.rev) != generation {
		// A mutation (rotate/revoke/scope change) raced this verification;
		// fail closed rather than publish a stale grant.
		return ManagementTokenIdentity{}, ErrManagementTokenInvalid
	}
	a.ensureCacheRoomLocked(now)
	a.entries[cacheKey] = managementCacheEntry{identity: identity, expires: now.Add(a.ttl)}
	return identity, nil
}

func (a *ManagementTokenAuthenticator) ensureCacheRoomLocked(now time.Time) {
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

// SweepExpired removes expired entries from the verified-token cache.
func (a *ManagementTokenAuthenticator) SweepExpired() {
	a.mu.Lock()
	a.sweepExpiredLocked(time.Now())
	a.mu.Unlock()
}

func (a *ManagementTokenAuthenticator) sweepExpiredLocked(now time.Time) {
	for key, entry := range a.entries {
		if !now.Before(entry.expires) {
			delete(a.entries, key)
		}
	}
}

// StartSweeper periodically evicts expired cache entries until ctx is done.
func (a *ManagementTokenAuthenticator) StartSweeper(ctx context.Context) {
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

// Invalidate drops every cached entry for one token.
func (a *ManagementTokenAuthenticator) Invalidate(tokenID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.invalidateLocked(tokenID)
}

// InvalidateWith publishes a token mutation while cache publication is
// excluded, so a verification begun against the old database state cannot
// authorize after the mutation commits.
func (a *ManagementTokenAuthenticator) InvalidateWith(tokenID string, publish func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := publish(); err != nil {
		return err
	}
	a.invalidateLocked(tokenID)
	return nil
}

// InvalidateAll drops the entire cache, e.g. after revoke-all.
func (a *ManagementTokenAuthenticator) InvalidateAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	atomic.AddUint64(&a.rev, 1)
	a.entries = make(map[[32]byte]managementCacheEntry)
}

func (a *ManagementTokenAuthenticator) invalidateLocked(tokenID string) {
	atomic.AddUint64(&a.rev, 1)
	for key, entry := range a.entries {
		if entry.identity.ID == tokenID {
			delete(a.entries, key)
		}
	}
}

func (a *ManagementTokenAuthenticator) cacheKey(raw string) [32]byte {
	h := hmac.New(sha256.New, a.key)
	_, _ = h.Write([]byte(raw))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
