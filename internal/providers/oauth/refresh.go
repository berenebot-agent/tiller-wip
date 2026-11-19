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
	errRefreshSuperseded = errors.New("oauth refresh superseded")
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
	done          chan struct{}
	record        TokenRecord
	err           error
	forced        bool
	forcedWaiters int
	// started bounds how long other callers will wait on this call. A refresh
	// whose goroutine never finishes must not pin every later waiter: after
	// this deadline the entry is treated as abandoned and a fresh refresh is
	// started by the next caller.
	started time.Time
}

// callWaitTimeout bounds how long a second caller waits on another caller's
// in-flight token refresh before giving up on it. The refresh itself runs
// under its caller's context, so this is deliberately several times longer
// than a normal token round-trip: it is a leak guard, not a latency budget.
var callWaitTimeout = 2 * time.Minute

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
	return m.refresh(ctx, accountID, providerID, refresh, false)
}

func (m *Manager) ForceRefresh(ctx context.Context, accountID, providerID string, refresh RefreshFunc) (TokenRecord, error) {
	return m.refresh(ctx, accountID, providerID, refresh, true)
}

func (m *Manager) refresh(ctx context.Context, accountID, providerID string, refresh RefreshFunc, force bool) (TokenRecord, error) {
	if refresh == nil {
		return TokenRecord{}, errors.New("oauth refresh function is required")
	}
	if err := ctx.Err(); err != nil {
		return TokenRecord{}, err
	}
	key := accountID + "\x00" + providerID
	m.mu.Lock()
	if call := m.calls[key]; call != nil {
		// An entry older than the wait timeout is presumed abandoned: its
		// goroutine never published a result, so waiting on it (or refusing to
		// start a fresh refresh because of it) would wedge every caller — and
		// with it the quota poller's in-flight state. Replace it instead.
		if wait := callWaitTimeout - time.Since(call.started); wait > 0 {
			if force {
				call.forcedWaiters++
			}
			m.mu.Unlock()
			var err error
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-call.done:
				return call.record, call.err
			case <-time.After(wait):
				err = context.DeadlineExceeded
			}
			if force {
				m.mu.Lock()
				call.forcedWaiters--
				m.mu.Unlock()
			}
			return TokenRecord{}, err
		}
	}
	call := &refreshCall{done: make(chan struct{}), forced: force, started: time.Now()}
	m.calls[key] = call
	m.mu.Unlock()

	record, err := m.get(ctx, accountID, providerID)
	loaded := err == nil
	generation := record.Generation
	var rotated *TokenRecord
	if err == nil {
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
					rotated = &merged
				}
			}
		} else {
			record, err = MergeToken(record, response, time.Now())
		}
	}

	// Keep ownership through the local write so replacement cannot race it.
	// The upstream refresh runs outside the lock.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls[key] == call {
		scope := m.store.For(accountID)
		if err == nil {
			record.Generation = generation
			err = scope.PutOAuthTokenIfGeneration(ctx, TokenToStore(record), generation)
		} else if rotated != nil {
			// Best effort, since the refresh is already failing.
			_ = scope.PutOAuthTokenIfGeneration(ctx, TokenToStore(*rotated), generation)
		}
		if loaded && (call.forced || call.forcedWaiters > 0) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			switch {
			case errors.Is(err, ErrReconnectRequired):
				_ = scope.SetOAuthStateIfGeneration(ctx, providerID, string(AuthReconnectRequired), time.Now(), generation)
			case errors.Is(err, ErrAuthUnavailable):
				_ = scope.SetOAuthStateIfGeneration(ctx, providerID, string(AuthUnavailable), time.Now(), generation)
			}
		}
		delete(m.calls, key)
	} else {
		record, err = TokenRecord{}, errRefreshSuperseded
	}
	call.record, call.err = record, err
	close(call.done)
	return record, err
}

func (m *Manager) get(ctx context.Context, accountID, providerID string) (TokenRecord, error) {
	row, err := m.store.For(accountID).GetOAuthToken(ctx, providerID)
	if err != nil {
		return TokenRecord{}, err
	}
	return TokenFromStore(row), nil
}
