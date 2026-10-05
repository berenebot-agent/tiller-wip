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
	// including on-view refreshes. It prevents an admin reload from stampeding
	// the provider endpoint.
	MinInterval = 30 * time.Second
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
		p.gen[k]++
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

// RefreshIfDue polls a provider when it is due under the cadence, and returns
// the (possibly cached) snapshot. force requests an immediate poll subject to
// MinInterval (used by the on-view refresh).
func (p *Poller) RefreshIfDue(ctx context.Context, accountID, providerID string, force bool, now time.Time) (Snapshot, bool) {
	k := key(accountID, providerID)
	p.mu.Lock()
	ref, ok := p.refs[k]
	if !ok {
		p.mu.Unlock()
		return Snapshot{}, false
	}
	interval := BackgroundInterval
	if p.activeWithin(k, now) {
		interval = ActiveInterval
	}
	last := p.last[k]
	due := last.IsZero() || now.Sub(last) >= interval
	if force {
		due = last.IsZero() || now.Sub(last) >= MinInterval
	}
	if !due || p.inflight[k] {
		cached := p.snap[k]
		p.mu.Unlock()
		return cached, false
	}
	p.inflight[k] = true
	// Capture the registration generation. If the provider is unregistered or
	// re-registered while this poll is in flight, the result must be discarded
	// rather than republished into snap, which would resurrect a snapshot for a
	// provider that no longer exists (or publish stale data over a new
	// registration).
	generation := p.gen[k]
	p.mu.Unlock()

	snap := p.poll(ctx, ref, now)

	p.mu.Lock()
	p.inflight[k] = false
	if p.gen[k] == generation {
		p.last[k] = now
		p.snap[k] = snap
	} else {
		// Registration changed mid-poll: drop the result and any cached state
		// so a subsequent Snapshot call does not serve the obsolete value.
		snap = p.snap[k]
	}
	p.mu.Unlock()
	return snap, true
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
