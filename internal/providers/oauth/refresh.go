package oauth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tiller-router/tiller-router/internal/store"
)

var (
	ErrReconnectRequired = errors.New("oauth reconnection required")
	ErrAuthUnavailable   = errors.New("oauth provider unavailable")
)

// PersistableRefreshError reports a refresh that rotated durable credentials and
// then failed on a later step. Token carries the rotated credentials, which must
// be persisted even though the refresh is reported as failed and remains
// retryable.
//
// It exists because a provider may invalidate the old credentials the moment
// the rotation succeeds. If the failure that follows is transient and the
// rotated pair is discarded, only the now-dead credentials remain stored and the
// next attempt is forced into a full reconnect. Err unwraps to the underlying
// transient failure so existing classification still applies.
type PersistableRefreshError struct {
	Token TokenResponse
	Err   error
}

func (e *PersistableRefreshError) Error() string { return e.Err.Error() }
func (e *PersistableRefreshError) Unwrap() error { return e.Err }

type RefreshFunc func(context.Context, TokenRecord) (TokenResponse, error)

type refreshCall struct {
	done   chan struct{}
	record TokenRecord
	err    error
}

type Manager struct {
	store *store.Store
	lead  time.Duration
	mu    sync.Mutex
	calls map[string]*refreshCall
}

func NewManager(st *store.Store, refreshLead time.Duration) *Manager {
	return &Manager{store: st, lead: refreshLead, calls: make(map[string]*refreshCall)}
}

func (m *Manager) Current(ctx context.Context, accountID, providerID string, refresh RefreshFunc) (TokenRecord, error) {
	record, err := m.get(ctx, accountID, providerID)
	if errors.Is(err, store.ErrNoOAuthToken) {
		return TokenRecord{}, ErrNoToken
	}
	if err != nil {
		return TokenRecord{}, err
	}
	switch Classify(record, time.Now()) {
	case AuthReconnectRequired:
		return TokenRecord{}, ErrReconnectRequired
	case AuthUnavailable:
		return TokenRecord{}, ErrAuthUnavailable
	}
	if !RefreshNeeded(record, time.Now(), m.lead) {
		return record, nil
	}
	if record.RefreshToken == "" {
		return TokenRecord{}, ErrReconnectRequired
	}
	return m.refresh(ctx, accountID, providerID, refresh)
}

func (m *Manager) ForceRefresh(ctx context.Context, accountID, providerID string, refresh RefreshFunc) (TokenRecord, error) {
	record, err := m.refresh(ctx, accountID, providerID, refresh)
	if err == nil {
		return record, nil
	}
	// Only transition to a dead state on a definitive signal. Transient
	// failures (network blips, timeouts, context cancellation) must remain
	// retryable — they never permanently mark the provider unavailable.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return record, err
	}
	now := time.Now()
	switch {
	case errors.Is(err, ErrReconnectRequired):
		_ = m.store.For(accountID).SetOAuthState(ctx, providerID, string(AuthReconnectRequired), now)
	case errors.Is(err, ErrAuthUnavailable):
		_ = m.store.For(accountID).SetOAuthState(ctx, providerID, string(AuthUnavailable), now)
	}
	return record, err
}

func (m *Manager) refresh(ctx context.Context, accountID, providerID string, refresh RefreshFunc) (TokenRecord, error) {
	if refresh == nil {
		return TokenRecord{}, errors.New("oauth refresh function is required")
	}
	key := accountID + "\x00" + providerID
	m.mu.Lock()
	if call := m.calls[key]; call != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return TokenRecord{}, ctx.Err()
		case <-call.done:
			return call.record, call.err
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	m.calls[key] = call
	m.mu.Unlock()

	record, err := m.get(ctx, accountID, providerID)
	var generation int64
	if err == nil {
		generation = record.Generation
		response, refreshErr := refresh(ctx, record)
		if refreshErr != nil {
			err = refreshErr
			// A refresh may rotate durable credentials and then fail on a later
			// step (for example GitHub rotates the token pair, then the derived
			// Copilot fetch fails). Persist those rotated credentials now: the
			// provider may already have invalidated the old pair, so discarding
			// them would force a reconnect after an otherwise transient error.
			var persistable *PersistableRefreshError
			if errors.As(refreshErr, &persistable) {
				if merged, mergeErr := MergeToken(record, persistable.Token, time.Now()); mergeErr == nil {
					merged.Generation = generation
					if putErr := m.store.For(accountID).PutOAuthTokenIfGeneration(ctx, TokenToStore(merged), generation); putErr != nil {
						// Best effort: the refresh is already failing. A
						// generation change here means the connection was
						// deliberately disconnected or replaced, which must not
						// be undone by this write.
						_ = putErr
					}
				}
			}
		} else {
			record, err = MergeToken(record, response, time.Now())
			if err == nil {
				record.Generation = generation
				err = m.store.For(accountID).PutOAuthTokenIfGeneration(ctx, TokenToStore(record), generation)
			}
		}
	}

	m.mu.Lock()
	call.record, call.err = record, err
	delete(m.calls, key)
	close(call.done)
	m.mu.Unlock()
	return record, err
}

func (m *Manager) get(ctx context.Context, accountID, providerID string) (TokenRecord, error) {
	row, err := m.store.For(accountID).GetOAuthToken(ctx, providerID)
	if err != nil {
		return TokenRecord{}, err
	}
	return TokenFromStore(row), nil
}
