package oauth

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrReconnectRequired = errors.New("oauth reconnection required")
	ErrAuthUnavailable   = errors.New("oauth provider unavailable")
)

type RefreshFunc func(context.Context, TokenRecord) (TokenResponse, error)

type refreshCall struct {
	done   chan struct{}
	record TokenRecord
	err    error
}

type Manager struct {
	store *Store
	lead  time.Duration
	mu    sync.Mutex
	calls map[string]*refreshCall
}

func NewManager(store *Store, refreshLead time.Duration) *Manager {
	return &Manager{store: store, lead: refreshLead, calls: make(map[string]*refreshCall)}
}

func (m *Manager) Current(ctx context.Context, providerID string, refresh RefreshFunc) (TokenRecord, error) {
	record, err := m.store.Get(ctx, providerID)
	if errors.Is(err, ErrNoToken) {
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
	return m.refresh(ctx, providerID, refresh)
}

func (m *Manager) ForceRefresh(ctx context.Context, providerID string, refresh RefreshFunc) (TokenRecord, error) {
	record, err := m.refresh(ctx, providerID, refresh)
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
		_ = m.store.SetState(ctx, providerID, AuthReconnectRequired, now)
	case errors.Is(err, ErrAuthUnavailable):
		_ = m.store.SetState(ctx, providerID, AuthUnavailable, now)
	}
	return record, err
}

func (m *Manager) refresh(ctx context.Context, providerID string, refresh RefreshFunc) (TokenRecord, error) {
	if refresh == nil {
		return TokenRecord{}, errors.New("oauth refresh function is required")
	}
	m.mu.Lock()
	if call := m.calls[providerID]; call != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return TokenRecord{}, ctx.Err()
		case <-call.done:
			return call.record, call.err
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	m.calls[providerID] = call
	m.mu.Unlock()

	record, err := m.store.Get(ctx, providerID)
	if err == nil {
		response, refreshErr := refresh(ctx, record)
		if refreshErr != nil {
			err = refreshErr
		} else {
			record, err = MergeToken(record, response, time.Now())
			if err == nil {
				err = m.store.Put(ctx, record)
			}
		}
	}

	m.mu.Lock()
	call.record, call.err = record, err
	delete(m.calls, providerID)
	close(call.done)
	m.mu.Unlock()
	return record, err
}
