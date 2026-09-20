package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tiller-router/tiller-router/internal/store"
)

// The asynchronous Activity writer keeps the request path off the database
// write critical path. Finished requests enqueue a prepared insert and move on;
// a background goroutine batches rows per account and writes each account's
// batch in one transaction. Logging is best-effort: if the queue is full, rows
// are dropped rather than blocking the request.
const (
	logWriteQueueSize     = 1024
	logWriteBatchSize     = 64
	logWriteFlushInterval = 25 * time.Millisecond
)

type logWrite struct {
	accountID string
	row       store.RequestLogInsert
}

// logWriter batches Activity inserts per account on a background goroutine.
type logWriter struct {
	ch       chan logWrite
	scopeFor func(string) *store.Scope
	logger   *slog.Logger

	dropped     atomic.Int64
	stopped     atomic.Int64
	lastDropLog atomic.Int64
	done        chan struct{}
	flushCh     chan chan struct{}
	stopCh      chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
}

func newLogWriter(scopeFor func(string) *store.Scope, logger *slog.Logger) *logWriter {
	return &logWriter{
		ch:       make(chan logWrite, logWriteQueueSize),
		scopeFor: scopeFor,
		logger:   logger,
		done:     make(chan struct{}),
		flushCh:  make(chan chan struct{}),
		stopCh:   make(chan struct{}),
	}
}

// enqueue offers a row to the writer without blocking. A full queue drops the
// row and logs a throttled warning.
func (w *logWriter) enqueue(item logWrite) {
	select {
	case <-w.stopCh:
		w.stopped.Add(1)
		return
	default:
	}
	select {
	case <-w.stopCh:
		w.stopped.Add(1)
		return
	case w.ch <- item:
	default:
		n := w.dropped.Add(1)
		now := time.Now().UnixNano()
		last := w.lastDropLog.Load()
		if now-last > int64(time.Minute) && w.lastDropLog.CompareAndSwap(last, now) && w.logger != nil {
			w.logger.Warn("activity log queue full; dropping rows", "dropped_total", n)
		}
	}
}

// stop stops accepting rows, drains the queue, flushes it, and waits for the
// writer. It is idempotent and safe for shutdown callers.
func (w *logWriter) stop(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.stopOnce.Do(func() { close(w.stopCh) })
	if err := w.flush(ctx); err != nil {
		return err
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// start launches the background batching goroutine. The writer drains and
// flushes queued rows when ctx is cancelled.
func (w *logWriter) start(ctx context.Context) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer close(w.done)
		w.run(ctx)
	}()
}

func (w *logWriter) run(ctx context.Context) {
	ticker := time.NewTicker(logWriteFlushInterval)
	defer ticker.Stop()
	pending := map[string][]store.RequestLogInsert{}
	total := 0
	flush := func() {
		for account, rows := range pending {
			if err := w.scopeFor(account).InsertRequestLogs(context.Background(), rows); err != nil && w.logger != nil {
				w.logger.Warn("activity batch write failed", "error_class", fmt.Sprintf("%T", err))
			}
			delete(pending, account)
		}
		total = 0
	}
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case item := <-w.ch:
					pending[item.accountID] = append(pending[item.accountID], item.row)
				default:
					flush()
					return
				}
			}
		case <-w.stopCh:
			for {
				select {
				case item := <-w.ch:
					pending[item.accountID] = append(pending[item.accountID], item.row)
				default:
					flush()
					return
				}
			}
		case item := <-w.ch:
			pending[item.accountID] = append(pending[item.accountID], item.row)
			total++
			if total >= logWriteBatchSize {
				flush()
			}
		case ack := <-w.flushCh:
			flush()
			close(ack)
		case <-ticker.C:
			if total > 0 {
				flush()
			}
		}
	}
}

// flush waits until all rows queued before the call are committed. Inference
// stays asynchronous, while an immediate Activity read (for example the
// dashboard after a request) observes the completed request deterministically.
func (w *logWriter) flush(ctx context.Context) error {
	if w == nil {
		return nil
	}
	ack := make(chan struct{})
	select {
	case w.flushCh <- ack:
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// wait blocks until the writer goroutine has stopped. Used by tests and
// shutdown.
func (w *logWriter) wait() {
	if w == nil {
		return
	}
	w.wg.Wait()
}
