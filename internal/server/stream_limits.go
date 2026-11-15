package server

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/tiller-router/tiller-router/internal/config"
)

const (
	codeStreamTimeLimit  = "stream_time_limit_exceeded"
	codeAccountSuspended = "account_suspended"
)

var (
	errStreamTimeLimit  = errors.New("inference request exceeded the maximum duration")
	errAccountSuspended = errors.New("account suspended")
)

// limitErrorDetail returns the HTTP status and client message for the errors
// that end a request because of a Tiller limit rather than an upstream fault.
func limitErrorDetail(code string) (status int, message string, ok bool) {
	switch code {
	case codeStreamTimeLimit:
		return http.StatusGatewayTimeout, "This request exceeded the maximum time allowed for a single response. Try a shorter request or a faster model.", true
	case codeAccountSuspended:
		return http.StatusForbidden, "This account has been suspended or is no longer active.", true
	}
	return 0, "", false
}

// streamRegistry tracks the cancel function of every in-flight inference
// request per account so a suspension can end them immediately.
type streamRegistry struct {
	mu      sync.Mutex
	next    uint64
	cancels map[string]map[uint64]context.CancelCauseFunc
}

func (g *streamRegistry) add(accountID string, cancel context.CancelCauseFunc) (remove func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancels == nil {
		g.cancels = map[string]map[uint64]context.CancelCauseFunc{}
	}
	if g.cancels[accountID] == nil {
		g.cancels[accountID] = map[uint64]context.CancelCauseFunc{}
	}
	g.next++
	id := g.next
	g.cancels[accountID][id] = cancel
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		delete(g.cancels[accountID], id)
		if len(g.cancels[accountID]) == 0 {
			delete(g.cancels, accountID)
		}
	}
}

func (g *streamRegistry) cancelAccount(accountID string) {
	g.mu.Lock()
	cancels := g.cancels[accountID]
	delete(g.cancels, accountID)
	g.mu.Unlock()
	for _, cancel := range cancels {
		cancel(errAccountSuspended)
	}
}

// cancelAccountStreams ends every in-flight inference request of the account.
func (s *Server) cancelAccountStreams(accountID string) {
	s.streams.cancelAccount(accountID)
}

// limitRequest applies the hosted request time limit and registers the request
// for cancellation on suspension. The returned function must be deferred.
func (s *Server) limitRequest(r *http.Request, accountID string) (*http.Request, func()) {
	if s.config.Mode != config.ModeHosted && s.config.MaxStreamDuration <= 0 {
		return r, func() {}
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	stopTimer := func() {}
	if d := s.config.MaxStreamDuration; d > 0 {
		timeoutCtx, cancelTimeout := context.WithTimeoutCause(ctx, d, errStreamTimeLimit)
		ctx, stopTimer = timeoutCtx, cancelTimeout
	}
	remove := s.streams.add(accountID, cancel)
	return r.WithContext(ctx), func() {
		remove()
		stopTimer()
		cancel(context.Canceled)
	}
}
