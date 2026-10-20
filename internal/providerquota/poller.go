package providerquota

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Cadence parameters. They are exported as variables so tests can shrink them.
var (
	// BackgroundInterval is the floor between polls for an idle provider.
	BackgroundInterval = 30 * time.Minute
	// ActiveInterval is the interval while the provider has served traffic
	// recently.
	ActiveInterval = 60 * time.Second
	// MinInterval is the shortest gap between two polls of one provider,
	// including on-view refreshes. This is the minimum interval even after
	// an unsuccessful quota check.
	MinInterval = 60 * time.Second
	// FailureRetry is how soon a provider whose last poll failed is retried,
	// overriding the normal (background/active/forced) interval. A transient
	// failure uses the same 60-second floor as successful checks.
	FailureRetry = 60 * time.Second
	// ActiveWindow is how recently a provider must have served traffic to be
	// considered active.
	ActiveWindow = time.Minute
)

// ProviderRef identifies a provider to poll.
type ProviderRef struct {
	AccountID  string
	ProviderID string
	Name       string
	Type       string
	BaseURL    string
	// AccountIDHeader is an optional provider-account header value (Codex).
	AccountIDHeader string
}

// Hydrate returns the current credential for a provider, or an error when it
// cannot be obtained. The server supplies this so the poller has no direct
// dependency on the store or OAuth manager.
type Hydrate func(ctx context.Context, ref ProviderRef) (string, error)

// Poller caches quota snapshots per account+provider and refreshes them on the
// configured cadence. It is safe for concurrent use.
type Poller struct {
	client  *http.Client
	hydrate Hydrate

	mu   sync.Mutex
	last map[string]time.Time
	snap map[string]Snapshot
	refs map[string]ProviderRef
	// gen increments for a provider on every register/unregister. A poll
	// captures the generation before its network work and only publishes if the
	// generation is unchanged, so a poll that was already in flight cannot
	// resurrect a snapshot for a provider that has since been removed or
	// changed.
	gen map[string]uint64
	// inflight prevents two concurrent polls of the same provider.
	inflight map[string]bool
	// active records the last time a provider served a request, so the cadence
	// can tighten to ActiveInterval while it is in use. It is in-memory only and
	// resets on restart, matching the live/telemetry posture.
	active map[string]time.Time
	// failed marks a provider whose most recent poll produced no usable
	// snapshot, so it is retried on the short FailureRetry cadence instead of a
	// full background/active interval. In-memory only, like active.
	failed map[string]bool
}

func NewPoller(client *http.Client, hydrate Hydrate) *Poller {
	return &Poller{
		client:   client,
		hydrate:  hydrate,
		last:     map[string]time.Time{},
		snap:     map[string]Snapshot{},
		refs:     map[string]ProviderRef{},
		gen:      map[string]uint64{},
		inflight: map[string]bool{},
		active:   map[string]time.Time{},
		failed:   map[string]bool{},
	}
}

// MarkActive records that a provider served a request, tightening its poll
// cadence to ActiveInterval for the ActiveWindow. It is called on the request
// path and never blocks.
func (p *Poller) MarkActive(accountID, providerID string, now time.Time) {
	if p == nil || providerID == "" {
		return
	}
	k := key(accountID, providerID)
	p.mu.Lock()
	p.active[k] = now
	p.mu.Unlock()
}

// activeWithin reports whether a provider served a request within ActiveWindow.
// It assumes p.mu is held.
func (p *Poller) activeWithin(k string, now time.Time) bool {
	t, ok := p.active[k]
	return ok && now.Sub(t) <= ActiveWindow
}

func key(accountID, providerID string) string { return accountID + "\x00" + providerID }

func (p *Poller) SetAccountHeader(accountID, providerID, header string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := key(accountID, providerID)
	ref := p.refs[k]
	ref.AccountIDHeader = header
	p.refs[k] = ref
}

// Register records a provider the poller may poll. It does not fetch; the
// server calls Refresh or RefreshIfDue. Type is only registered when an adapter
// exists.
func (p *Poller) Register(ref ProviderRef) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.registerLocked(ref)
}

func (p *Poller) registerLocked(ref ProviderRef) {
	if !Supports(ref.Type) {
		return
	}
	k := key(ref.AccountID, ref.ProviderID)
	if existing, ok := p.refs[k]; ok && existing == ref {
		// An unchanged registration must not invalidate an in-flight poll.
		return
	}
	p.refs[k] = ref
	p.gen[k]++
}

// Reconcile replaces the registered set with refs, registering new providers
// and unregistering any provider no longer present. Without this, a deleted or
// disabled provider kept its registration and its cached snapshot forever,
// because registration was add-only. The whole diff runs under one lock so a
// concurrent poll cannot observe a half-reconciled set.
func (p *Poller) Reconcile(refs []ProviderRef) {
	want := make(map[string]ProviderRef, len(refs))
	for _, ref := range refs {
		if Supports(ref.Type) {
			want[key(ref.AccountID, ref.ProviderID)] = ref
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, ref := range want {
		if existing, ok := p.refs[k]; ok && existing == ref {
			continue
		}
		p.refs[k] = ref
		p.gen[k]++
	}
	for k := range p.refs {
		if _, ok := want[k]; ok {
			continue
		}
		delete(p.refs, k)
		delete(p.snap, k)
		delete(p.last, k)
		delete(p.inflight, k)
		delete(p.failed, k)
		p.gen[k]++
	}
}

// ReconcileAccount changes only one account's registered quota providers.
// A different tenant's browser cannot invalidate cached values or live checks.
func (p *Poller) ReconcileAccount(accountID string, refs []ProviderRef) {
	want := make(map[string]ProviderRef, len(refs))
	for _, ref := range refs {
		if ref.AccountID == accountID && Supports(ref.Type) {
			want[key(accountID, ref.ProviderID)] = ref
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, ref := range want {
		if previous, ok := p.refs[k]; ok {
			// OAuth hydration may attach an account header.
			ref.AccountIDHeader = previous.AccountIDHeader
			if previous == ref {
				continue
			}
		}
		p.refs[k] = ref
		p.gen[k]++
		delete(p.snap, k)
		delete(p.last, k)
		delete(p.failed, k)
		delete(p.inflight, k)
		delete(p.active, k)
	}
	prefix := accountID + "\x00"
	for k := range p.refs {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			if _, keep := want[k]; !keep {
				delete(p.refs, k)
				delete(p.snap, k)
				delete(p.last, k)
				delete(p.failed, k)
				delete(p.inflight, k)
				delete(p.active, k)
				p.gen[k]++
			}
		}
	}
}

// Unregister drops a provider's registration and cached snapshot.
func (p *Poller) Unregister(accountID, providerID string) {
	k := key(accountID, providerID)
	p.mu.Lock()
	delete(p.refs, k)
	delete(p.snap, k)
	delete(p.last, k)
	delete(p.inflight, k)
	delete(p.failed, k)
	p.gen[k]++
	p.mu.Unlock()
}

// Snapshot returns the cached snapshot for a provider, if any.
func (p *Poller) Snapshot(accountID, providerID string) (Snapshot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.snap[key(accountID, providerID)]
	return s, ok
}

// SnapshotForAccount returns every cached snapshot for an account keyed by
// provider id.
func (p *Poller) SnapshotForAccount(accountID string) map[string]Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	prefix := accountID + "\x00"
	out := map[string]Snapshot{}
	for k, s := range p.snap {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out[k[len(prefix):]] = s
		}
	}
	return out
}

// PollStatus keeps the last attempt separate from the last successful result.
// A failed refresh must not make stale quota figures appear current.
type PollStatus struct {
	Refreshing    bool       `json:"refreshing"`
	Failed        bool       `json:"failed"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
}

// PollStatesForAccount reports only the current account's quota status.
func (p *Poller) PollStatesForAccount(accountID string) map[string]PollStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make(map[string]PollStatus)
	prefix := accountID + "\x00"
	for k := range p.refs {
		if len(k) < len(prefix) || k[:len(prefix)] != prefix {
			continue
		}
		status := PollStatus{Refreshing: p.inflight[k], Failed: p.failed[k]}
		if last := p.last[k]; !last.IsZero() {
			value := last.UTC()
			status.LastAttemptAt = &value
		}
		if snap := p.snap[k]; snap.Available && !snap.FetchedAt.IsZero() {
			value := snap.FetchedAt.UTC()
			status.LastSuccessAt = &value
		}
		result[k[len(prefix):]] = status
	}
	return result
}

// Reserve before upstream I/O starts. A second attempt cannot begin while a
// previous request is running or within the 60-second interval.
func (p *Poller) beginRefresh(accountID, providerID string, force bool, now time.Time) (ProviderRef, uint64, bool) {
	k := key(accountID, providerID)
	p.mu.Lock()
	defer p.mu.Unlock()
	ref, ok := p.refs[k]
	if !ok || p.inflight[k] {
		return ProviderRef{}, 0, false
	}
	interval := BackgroundInterval
	if p.activeWithin(k, now) {
		interval = ActiveInterval
	}
	if p.failed[k] {
		interval = FailureRetry
	}
	if force {
		interval = MinInterval
	}
	if last := p.last[k]; !last.IsZero() && now.Sub(last) < interval {
		return ProviderRef{}, 0, false
	}
	p.inflight[k] = true
	p.last[k] = now
	return ref, p.gen[k], true
}

func (p *Poller) finishRefresh(ctx context.Context, ref ProviderRef, generation uint64, now time.Time) Snapshot {
	snap := p.poll(ctx, ref, now)
	k := key(ref.AccountID, ref.ProviderID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gen[k] == generation {
		p.inflight[k] = false
		// Retain the last successful snapshot if a later check fails.
		if snap.Available || !p.snap[k].Available {
			p.snap[k] = snap
		}
		p.failed[k] = !snap.Available
	}
	return p.snap[k]
}

// RefreshIfDue retains a synchronous API for callers and test harnesses.
func (p *Poller) RefreshIfDue(ctx context.Context, accountID, providerID string, force bool, now time.Time) (Snapshot, bool) {
	ref, generation, ok := p.beginRefresh(accountID, providerID, force, now)
	if !ok {
		cached, _ := p.Snapshot(accountID, providerID)
		return cached, false
	}
	return p.finishRefresh(ctx, ref, generation, now), true
}

// Bound simultaneous quota checks across all users to eight. At saturation,
// skip a request rather than queueing unbounded goroutines.
var quotaRefreshGate = make(chan struct{}, 8)

// TriggerIfDue starts an eligible check asynchronously so cached quota and a
// per-provider spinner can be shown without waiting for the upstream.
func (p *Poller) TriggerIfDue(accountID, providerID string, now time.Time) bool {
	select {
	case quotaRefreshGate <- struct{}{}:
	default:
		return false
	}
	ref, generation, ok := p.beginRefresh(accountID, providerID, true, now)
	if !ok {
		<-quotaRefreshGate
		return false
	}
	go func() {
		defer func() { <-quotaRefreshGate }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p.finishRefresh(ctx, ref, generation, now)
	}()
	return true
}

// poll performs one fetch with credential hydration, converting any failure
// into an unavailable snapshot so callers never see an error that would block
// them.
func (p *Poller) poll(ctx context.Context, ref ProviderRef, now time.Time) Snapshot {
	snap := Snapshot{
		ProviderID:   ref.ProviderID,
		ProviderName: ref.Name,
		ProviderType: ref.Type,
		FetchedAt:    now.UTC(),
	}
	adapter, ok := adapters[ref.Type]
	if !ok {
		snap.Reason = "no quota adapter"
		return snap
	}
	token, err := p.hydrate(ctx, ref)
	if err != nil || token == "" {
		snap.Reason = "credentials unavailable"
		return snap
	}
	cred := Credential{
		AccountID:       ref.AccountID,
		ProviderID:      ref.ProviderID,
		Name:            ref.Name,
		Type:            ref.Type,
		BaseURL:         ref.BaseURL,
		Credential:      token,
		AccountIDHeader: ref.AccountIDHeader,
	}
	p.mu.Lock()
	cred.AccountIDHeader = p.refs[key(ref.AccountID, ref.ProviderID)].AccountIDHeader
	p.mu.Unlock()
	got, err := adapter(ctx, p.client, cred)
	if err != nil {
		snap.Reason = "quota endpoint unavailable"
		return snap
	}
	got.ProviderID = ref.ProviderID
	got.ProviderName = ref.Name
	got.ProviderType = ref.Type
	got.FetchedAt = now.UTC()
	got.Available = len(got.Windows) > 0
	if !got.Available {
		got.Reason = "quota format unavailable"
	}
	return got
}
