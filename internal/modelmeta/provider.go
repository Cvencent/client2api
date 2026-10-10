package modelmeta

import (
	"strings"
	"time"
)

// Options configures a Provider. Only Client is really needed; every other field
// has a safe default that keeps the provider offline and honest.
type Options struct {
	// Client selects this client's slice of the static table ("workbuddy", "kimi",
	// "tabbit", ...). An unknown client simply has no static entries.
	Client string
	// Overrides is the operator's manually edited model metadata. It is applied
	// last and therefore wins over both the live vendor value and the static
	// table.
	Overrides *OverrideStore
	// Realm selects the reasoning-effort vocabulary. Empty means the CN table,
	// matching the reference, which treats every realm except "global" as CN.
	Realm string
	// CacheDir enables the on-disk cache layer. Empty keeps the cache in memory
	// only, so a module that was not wired for persistence is unaffected.
	CacheDir string
	// Remote enables models.dev. Leave nil (or Enabled: false) to stay fully
	// offline — the default.
	Remote *RemoteConfig
	// ContextCap is the context policy value applied when nothing else knows the
	// window (workbuddy passes DefaultContextWindow to mirror the reference).
	// Zero disables the policy, so an unknown model honestly reports not-found
	// instead of a made-up number. An output cap is never applied: there is no
	// safe direction for guessing an output limit.
	ContextCap int64
	// Logf receives warnings about unreadable or unwritable cache files. Nil means
	// silent.
	Logf func(format string, args ...any)
}

// Provider resolves metadata for one client in one realm.
//
// It is safe for concurrent use. Resolution order is vendor value (when the caller
// supplies one) -> static table -> models.dev -> local cache, and the provenance of
// every field survives the merge so a caller can report where a number came from.
type Provider struct {
	client     string
	realm      string
	contextCap int64
	overrides  *OverrideStore
	store      *Store
	remote     *remoteCache
}

// New builds a Provider. It never fails and never blocks: with Remote unset the
// provider is entirely offline.
func New(opts Options) *Provider {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := &Provider{
		client:     strings.TrimSpace(opts.Client),
		realm:      NormalizeRealm(opts.Realm),
		contextCap: opts.ContextCap,
		overrides:  opts.Overrides,
		remote:     newRemote(opts.Remote),
	}
	if dir := strings.TrimSpace(opts.CacheDir); dir != "" {
		p.store = OpenStore(dir, p.client)
		p.store.SetWarnf(logf)
	}
	if opts.Remote != nil && !opts.Remote.Enabled {
		logf("modelmeta: models.dev layer disabled for client %q (opt-in required)", p.client)
	}
	return p
}

// Client returns the configured client key.
func (p *Provider) Client() string { return p.client }

// Realm returns the normalised realm key.
func (p *Provider) Realm() string { return p.realm }

// Store returns the cache layer, or nil when no cache directory was configured.
func (p *Provider) Store() *Store { return p.store }

// Lookup resolves metadata for a model with no vendor values at hand — the cold
// start and upstream-outage case. It returns false when nothing at all is known,
// which is the honest answer rather than a zero-value Meta.
func (p *Provider) Lookup(model string) (Meta, bool) {
	return p.resolve(model, Meta{})
}

// Merge fills the fields a vendor left empty and returns the result. A value the
// vendor supplied is always preserved, even when a source disagrees with it.
//
// This is the API a client module should use: it applies the whole precedence
// chain, so the module never duplicates the rules.
func (p *Provider) Merge(model string, vendor Meta) Meta {
	return p.MergeAt(model, vendor, time.Time{})
}

// MergeAt is Merge with an explicit request time for time-varying prices. A
// zero time selects the static off-peak/catalogue value; the gateway passes the
// completed request timestamp so DeepSeek peak rates apply to the right calls.
func (p *Provider) MergeAt(model string, vendor Meta, at time.Time) Meta {
	if vendor.Source == "" && !vendor.IsZero() {
		// The caller passed live values without provenance; they are vendor values
		// by definition.
		vendor.Source = SourceVendor
	}
	out, _ := p.resolveAt(model, vendor, at)
	return out
}

// WaitRemote blocks until the pending models.dev refresh settles or the timeout
// expires, reporting whether a document is available. Request paths must not call
// it; it exists for tests and for an explicit operator refresh.
func (p *Provider) WaitRemote(timeout time.Duration) bool {
	if p == nil || p.remote == nil {
		return false
	}
	return p.remote.wait(timeout)
}

// RemoteEnabled reports whether the models.dev layer is on.
func (p *Provider) RemoteEnabled() bool {
	return p != nil && p.remote.enabled()
}

// resolve is the whole chain in one place.
func (p *Provider) resolve(model string, vendor Meta) (Meta, bool) {
	return p.resolveAt(model, vendor, time.Time{})
}

func (p *Provider) resolveAt(model string, vendor Meta, at time.Time) (Meta, bool) {
	if p == nil {
		return vendor.Clone(), !vendor.IsZero()
	}
	out := vendor.Clone()
	if p.overrides != nil && model != "" {
		if o, ok := p.overrides.Get(p.client, model); ok {
			out = applyOverride(out, o)
		}
	}
	model = strings.TrimSpace(model)
	if model != "" {
		if e, ok := static().entry(p.client, model); ok {
			out = Merge(out, e.meta(p.realm))
		}
		if m, ok := p.remote.lookup(model); ok {
			out = Merge(out, m)
		} else if p.remote.enabled() && !p.remote.alreadyNegated(model) {
			// Fire and forget: the fetch may land after this call and serve the
			// next one. A request never waits on the network here.
			p.remote.refreshAsync()
			p.remote.noteMiss(model)
		}
		if p.store != nil {
			if m, ok := p.store.Get(model); ok {
				out = Merge(out, m)
			}
		}
		if !out.HasPrice || (!out.HasCacheRead && out.SourceOf(FieldInputPrice) != SourceManual) {
			if price, ok := DefaultPriceAt(p.client, model, at); ok {
				out = mergePrice(out, price, SourceStatic)
			}
		}

	}
	if !out.Has(FieldContextLength) && model != "" && p.contextCap > 0 {
		// Policy value: only the context window is over-estimated this way, and
		// only when a caller explicitly asked for a cap. It is tagged
		// SourceFallback so the operator can see it was not measured.
		out.ContextLength = p.contextCap
		if out.FieldSources == nil {
			out.FieldSources = map[string]string{}
		}
		out.FieldSources[FieldContextLength] = SourceFallback
		out.Source = strongestSource(out.FieldSources)
	}
	if hasCapabilityMetadata(out) && model != "" && p.store != nil {
		// Warm the cache so the next cold start has an answer even with no vendor
		// and no models.dev access. Put is a no-op when nothing changed.
		p.store.Put(model, out)
	}
	return out, hasCapabilityMetadata(out)
}

// hasCapabilityMetadata reports whether resolved data answers a capability
// question (context, output, efforts). Price alone is useful to the accounting
// path, but it is not a successful metadata lookup: a model with no published
// limits must still let the caller try the next metadata source or use its own
// default rather than treating a price row as a complete answer.
func hasCapabilityMetadata(m Meta) bool {
	return m.Has(FieldContextLength) || m.Has(FieldMaxOutputTokens) ||
		m.Has(FieldEfforts) || m.Has(FieldDefaultEffort)
}
