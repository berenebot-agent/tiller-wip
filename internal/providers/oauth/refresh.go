package oauth

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/tiller-router/tiller-router/internal/store"
)

var (
	ErrReconnectRequired = errors.New("oauth reconnection required")
	ErrAuthUnavailable   = errors.New("oauth provider unavailable")
	errRefreshSuperseded = errors.New("oauth refresh superseded")
)

// TokenEndpointError carries safe diagnostics from an OAuth token endpoint.
type TokenEndpointError struct {
	Status       int
	InvalidGrant bool
	Err          error
}

func (e *TokenEndpointError) Error() string { return e.Err.Error() }
func (e *TokenEndpointError) Unwrap() error { return e.Err }

func FailureReason(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, ErrNoToken), errors.Is(err, store.ErrNoOAuthToken):
		return "not_connected"
	case errors.Is(err, ErrReconnectRequired):
		return "reconnect_required"
	case errors.Is(err, ErrAuthUnavailable):
		return "unavailable"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errRefreshSuperseded), errors.Is(err, store.ErrOAuthGenerationChanged):
		return "superseded"
	default:
		return "error"
	}
}

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
	store  *store.Store
	lead   time.Duration
	logger *slog.Logger
	mu     sync.Mutex
	calls  map[string]*refreshCall
}

func NewManager(st *store.Store, refreshLead time.Duration, logger *slog.Logger) *Manager {
	return &Manager{store: st, lead: refreshLead, logger: logger, calls: make(map[string]*refreshCall)}
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
	return m.refresh(ctx, accountID, providerID, refresh, false, "")
}

func (m *Manager) ForceRefresh(ctx context.Context, accountID, providerID string, refresh RefreshFunc) (TokenRecord, error) {
	return m.refresh(ctx, accountID, providerID, refresh, true, "")
}

// RefreshRejected recovers from rejection of a specific access token. An
// already completed rotation can satisfy a caller holding the old token.
func (m *Manager) RefreshRejected(ctx context.Context, accountID, providerID string, refresh RefreshFunc, rejected string) (TokenRecord, error) {
	return m.refresh(ctx, accountID, providerID, refresh, true, rejected)
}

func (m *Manager) refresh(ctx context.Context, accountID, providerID string, refresh RefreshFunc, force bool, rejected string) (TokenRecord, error) {
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
	// Freshness re-check under the flight. Current() reads the token and only
	// then joins-or-starts a flight, so a caller descheduled in that window can
	// arrive after the flight that refreshed the token has already completed and
	// deleted itself. Without this check that caller starts a second, redundant
	// refresh of credentials that are already fresh.
	//
	// An explicit force refresh always reaches the token endpoint. Recovery
	// from rejection can reuse fresh credentials saved by another caller.
	if loaded && (!force || rejected != "") {
		switch Classify(record, time.Now()) {
		case AuthReconnectRequired:
			return m.finish(key, call, record, ErrReconnectRequired)
		case AuthUnavailable:
			return m.finish(key, call, record, ErrAuthUnavailable)
		}
		if (!force || record.AccessToken != rejected) && !RefreshNeeded(record, time.Now(), m.lead) {
			return m.finish(key, call, record, nil)
		}
	}
	generation := record.Generation
	var rotated *TokenRecord
	stage := "load"
	if err == nil {
		stage = "exchange"
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
	var stateErr error
	m.mu.Lock()
	if m.calls[key] == call {
		scope := m.store.For(accountID)
		if err == nil {
			stage = "persist"
			record.Generation = generation
			err = scope.PutOAuthTokenIfGeneration(ctx, TokenToStore(record), generation)
		} else if rotated != nil {
			// Best effort, since the refresh is already failing.
			_ = scope.PutOAuthTokenIfGeneration(ctx, TokenToStore(*rotated), generation)
		}
		if loaded && ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			var state AuthState
			switch {
			case errors.Is(err, ErrReconnectRequired):
				state = AuthReconnectRequired
			case errors.Is(err, ErrAuthUnavailable) && (call.forced || call.forcedWaiters > 0):
				state = AuthUnavailable
			}
			if state != "" {
				stateErr = scope.SetOAuthStateIfGeneration(ctx, providerID, string(state), time.Now(), generation)
				if stateErr == nil {
					record.AuthState = state
				}
			}
		}
		delete(m.calls, key)
	} else {
		record, err = TokenRecord{}, errRefreshSuperseded
	}
	call.record, call.err = record, err
	close(call.done)
	m.mu.Unlock()
	if stateErr != nil {
		m.logRefresh(ctx, accountID, providerID, force, "persist_state", stateErr)
	}
	m.logRefresh(ctx, accountID, providerID, force, stage, err)
	return record, err
}

func (m *Manager) logRefresh(ctx context.Context, accountID, providerID string, force bool, stage string, err error) {
	if m.logger == nil {
		return
	}
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{
		slog.String("account_id", accountID), slog.String("provider_id", providerID),
		slog.Bool("forced", force), slog.String("stage", stage), slog.String("outcome", FailureReason(err)),
	}
	var endpointErr *TokenEndpointError
	if errors.As(err, &endpointErr) {
		attrs = append(attrs, slog.Int("http_status", endpointErr.Status), slog.Bool("invalid_grant", endpointErr.InvalidGrant))
	}
	m.logger.LogAttrs(ctx, level, "provider OAuth token refresh", attrs...)
}

// finish publishes an already-known result for call and releases its flight.
// It is the path taken when the freshness re-check proves the refresh is no
// longer needed at all, so there is nothing to write.
func (m *Manager) finish(key string, call *refreshCall, record TokenRecord, err error) (TokenRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	call.record, call.err = record, err
	close(call.done)
	if m.calls[key] == call {
		delete(m.calls, key)
	}
	return record, err
}

func (m *Manager) get(ctx context.Context, accountID, providerID string) (TokenRecord, error) {
	row, err := m.store.For(accountID).GetOAuthToken(ctx, providerID)
	if err != nil {
		return TokenRecord{}, err
	}
	return TokenFromStore(row), nil
}
