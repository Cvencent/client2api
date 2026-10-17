package panel

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

// Per-client budget for a catalogue call.  The overall request budget below is
// slightly larger so that the last client to finish still has room to answer
// rather than being cut off by the outer deadline.
const (
	modelsClientTimeout = 5 * time.Second
	modelsTotalTimeout  = 8 * time.Second
)

type modelInfo struct {
	ID                      string         `json:"id"`
	OwnedBy                 string         `json:"owned_by"`
	Extra                   map[string]any `json:"extra"`
	ContextLength           int64          `json:"context_length,omitempty"`
	MaxOutputTokens         int64          `json:"max_output_tokens,omitempty"`
	ContextSource           string         `json:"context_source,omitempty"`
	MaxOutputSource         string         `json:"max_output_source,omitempty"`
	SupportedEfforts        []string       `json:"supported_efforts,omitempty"`
	DefaultEffort           string         `json:"default_effort,omitempty"`
	EffortsSource           string         `json:"efforts_source,omitempty"`
	DefaultEffortSource     string         `json:"default_effort_source,omitempty"`
	SupportedEffortsDefault []string       `json:"supported_efforts_default,omitempty"`
	DefaultEffortDefault    string         `json:"default_effort_default,omitempty"`
	ElevateTop              bool           `json:"elevate_top"`
	InputPerMillion         float64        `json:"input_per_million,omitempty"`
	OutputPerMillion        float64        `json:"output_per_million,omitempty"`
	CacheReadPerMillion     float64        `json:"cache_read_per_million,omitempty"`
	HasPrice                bool           `json:"has_price"`
	HasCacheRead            bool           `json:"has_cache_read"`
	InputPriceSource        string         `json:"input_price_source,omitempty"`
	OutputPriceSource       string         `json:"output_price_source,omitempty"`
	CacheReadPriceSource    string         `json:"cache_read_price_source,omitempty"`
	InputPriceDefault       float64        `json:"input_price_default,omitempty"`
	OutputPriceDefault      float64        `json:"output_price_default,omitempty"`
	CacheReadPriceDefault   float64        `json:"cache_read_price_default,omitempty"`
	DefaultHasPrice         bool           `json:"default_has_price"`
	DefaultHasCacheRead     bool           `json:"default_has_cache_read"`
	PriceEditable           bool           `json:"price_editable"`
	ContextEditable         bool           `json:"context_editable"`
	MaxOutputEditable       bool           `json:"max_output_editable"`
	EffortsEditable         bool           `json:"efforts_editable"`
	ElevateTopEditable      bool           `json:"elevate_top_editable"`
	// ElevateTopDefault is the embedded per-model policy with no operator
	// choice applied. The row shows it so "restore default" can bring back an
	// on-by-default model instead of silently writing a manual "off".
	ElevateTopDefault bool `json:"elevate_top_default"`
	// ElevateTopManual reports that the operator recorded an explicit
	// cap-alignment choice, so the row can say "manual" instead of "default".
	ElevateTopManual bool `json:"elevate_top_manual"`
	// ContextDefault / MaxOutputDefault is the value that would apply with no
	// manual override (upstream value, else the official preset). It is what the
	// row's "restore default" button writes back, and zero means unknown.
	ContextDefault   int64 `json:"context_default,omitempty"`
	MaxOutputDefault int64 `json:"max_output_default,omitempty"`
}

type clientModels struct {
	Name       string      `json:"name"`
	CanRefresh bool        `json:"can_refresh"`
	Error      string      `json:"error"`
	Models     []modelInfo `json:"models"`
}

type modelsReport struct {
	RefreshedAt string         `json:"refreshed_at"`
	Clients     []clientModels `json:"clients"`
}

// handleModelsList reports each module's catalogue as the module itself
// presents it.  Unlike /v1/models this is NOT flattened: the panel groups by
// client, so the client boundary is the whole point.
func (p *panel) handleModelsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	p.serveModels(w, r, false)
}

// handleModelsRefresh re-reads every catalogue, calling RefreshModels on the
// modules that actually implement it.  A module that does not implement it is
// served from its plain Models: claiming a refresh happened would be a lie.
func (p *panel) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	p.serveModels(w, r, true)
}

func (p *panel) serveModels(w http.ResponseWriter, r *http.Request, refresh bool) {
	totalBudget, clientBudget := modelsTotalTimeout, modelsClientTimeout
	if refresh && r.URL.Query().Get("client") != "" {
		totalBudget = 90 * time.Second
		clientBudget = 85 * time.Second
	}
	ctx, cancel := p.ctx(r, totalBudget)
	defer cancel()

	var clients []core.Client
	if p.opts.Registry != nil {
		clients = p.opts.Registry.All()
	}
	if name := r.URL.Query().Get("client"); name != "" {
		var selected core.Client
		if p.opts.Registry != nil {
			selected, _ = p.opts.Registry.Get(name)
		}
		if selected == nil {
			writeErr(w, http.StatusNotFound, "platform not found")
			return
		}
		clients = []core.Client{selected}
	}

	// Every client gets its own goroutine and its own slice slot, so a slow
	// module delays only itself and the results need no lock.
	out := make([]clientModels, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		p.safeGo("panel models", func() {
			defer wg.Done()
			cctx, ccancel := context.WithTimeout(ctx, clientBudget)
			defer ccancel()
			out[i] = p.modelsFor(cctx, c, refresh)
		})
	}
	wg.Wait()

	if r.URL.Query().Get("client") == "" {
		out = append(out, p.virtualModelsFor(out))
	}
	writeJSON(w, http.StatusOK, modelsReport{
		RefreshedAt: time.Now().UTC().Format(time.RFC3339),
		Clients:     out,
	})
}

// virtualModelsFor exposes the gateway-level routing names in the same
// editable table as platform models.  Their fallback window is the widest
// concrete member the name may reach; routing still trims per candidate.
func (p *panel) virtualModelsFor(real []clientModels) clientModels {
	row := clientModels{Name: core.VirtualModelClient, Models: []modelInfo{}}
	if p.opts.Registry == nil {
		return row
	}

	type pick struct {
		display  string
		raw      string
		context  int64
		output   int64
		efforts  []string
		default_ string
	}
	auto := map[string]pick{}
	// firstDefault keeps the default advertised by the first routed platform.
	// The aggregate window/efforts grow across platforms, but an omitted effort
	// must not drift to whichever member happened to be iterated last.
	firstDefault := map[string]string{}
	concrete := map[string]modelInfo{}
	concreteEfforts := map[string][]string{}
	concreteDefaults := map[string]string{}
	for _, c := range real {
		for _, m := range c.Models {
			concrete[c.Name+"\x00"+m.ID] = m
			concreteEfforts[c.Name+"\x00"+m.ID] = append([]string(nil), m.SupportedEfforts...)
			concreteDefaults[c.Name+"\x00"+m.ID] = m.DefaultEffort
			key := core.CanonicalModelID(m.ID)
			if key == "" {
				continue
			}
			display := core.ModelDisplayID(m.ID)
			if display == "" {
				display = m.ID
			}
			next := pick{
				display: display, raw: m.ID, context: m.ContextLength, output: m.MaxOutputTokens,
				efforts: append([]string(nil), m.SupportedEfforts...), default_: m.DefaultEffort,
			}
			if _, ok := firstDefault[key]; !ok {
				firstDefault[key] = m.DefaultEffort
			}
			if prev, ok := auto[key]; ok {
				if prev.context > next.context {
					next.context = prev.context
				}
				if prev.output > next.output {
					next.output = prev.output
				}
				next.efforts = dedupeEfforts(append(append([]string(nil), prev.efforts...), next.efforts...))
				if !betterVirtualDisplay(next.display, next.raw, prev.display, prev.raw) {
					next.display = prev.display
					next.raw = prev.raw
				}
			}
			auto[key] = next
		}
	}

	appendRow := func(id, ownedBy string, context, output int64, efforts []string, defaultEffort string, extra map[string]any) {
		contextSource, outputSource := "", ""
		if context > 0 {
			contextSource = modelmeta.SourceVendor
		}
		if output > 0 {
			outputSource = modelmeta.SourceVendor
		}
		effortsSource := ""
		if len(efforts) > 0 {
			effortsSource = modelmeta.SourceVendor
		}
		contextDefault, outputDefault := context, output
		effortsDefault := append([]string(nil), efforts...)
		defaultDefault := defaultEffort
		if p.opts.ModelOverrides != nil {
			if o, ok := p.opts.ModelOverrides.Get(core.VirtualModelClient, id); ok {
				if o.ContextLength > 0 {
					context = o.ContextLength
					contextSource = modelmeta.SourceManual
				}
				if o.MaxOutputTokens > 0 {
					output = o.MaxOutputTokens
					outputSource = modelmeta.SourceManual
				}
				if len(o.SupportedEfforts) > 0 {
					efforts = append([]string(nil), o.SupportedEfforts...)
					effortsSource = modelmeta.SourceManual
				}
				if o.DefaultEffort != "" && containsString(efforts, o.DefaultEffort) {
					defaultEffort = o.DefaultEffort
				}
			}
		}
		if defaultEffort != "" && !containsString(efforts, defaultEffort) {
			defaultEffort = ""
		}
		row.Models = append(row.Models, modelInfo{
			ID:               id,
			OwnedBy:          ownedBy,
			Extra:            extra,
			ContextLength:    context,
			MaxOutputTokens:  output,
			ContextSource:    contextSource,
			MaxOutputSource:  outputSource,
			ContextDefault:   contextDefault,
			MaxOutputDefault: outputDefault,
			SupportedEfforts: efforts,
			DefaultEffort:    defaultEffort,
			EffortsSource:    effortsSource,
			DefaultEffortSource: func() string {
				if defaultEffort != "" {
					return effortsSource
				}
				return ""
			}(),
			SupportedEffortsDefault: effortsDefault,
			DefaultEffortDefault:    defaultDefault,
			ContextEditable:         p.opts.ModelOverrides != nil,
			MaxOutputEditable:       p.opts.ModelOverrides != nil,
			EffortsEditable:         p.opts.ModelOverrides != nil,
		})
	}

	keys := make([]string, 0, len(auto))
	for key := range auto {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		pick := auto[key]
		pick.default_ = firstDefault[key]
		appendRow("Auto/"+pick.display, "auto", pick.context, pick.output, pick.efforts, pick.default_, map[string]any{
			"auto":   true,
			"target": key,
		})
	}

	groups := p.opts.Registry.ModelGroups()
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		group := groups[name]
		var context, output int64
		members := make([]string, 0, len(group.Members))
		var efforts []string
		defaultEffort := ""
		for _, member := range group.Members {
			qualified := member.Client + "/" + member.Model
			members = append(members, qualified)
			if strings.EqualFold(member.Client, "Auto") {
				if pick, ok := auto[core.CanonicalModelID(member.Model)]; ok {
					if pick.context > context {
						context = pick.context
					}
					if pick.output > output {
						output = pick.output
					}
					efforts = dedupeEfforts(append(efforts, pick.efforts...))
					if defaultEffort == "" && pick.default_ != "" {
						defaultEffort = pick.default_
					}
				}
				continue
			}
			if m, ok := concrete[member.Client+"\x00"+member.Model]; ok {
				if m.ContextLength > context {
					context = m.ContextLength
				}
				if m.MaxOutputTokens > output {
					output = m.MaxOutputTokens
				}
				efforts = dedupeEfforts(append(efforts, concreteEfforts[member.Client+"\x00"+member.Model]...))
				if defaultEffort == "" {
					defaultEffort = concreteDefaults[member.Client+"\x00"+member.Model]
				}
			}
		}
		appendRow(name, "group", context, output, efforts, defaultEffort, map[string]any{"members": members})
	}
	return row
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func dedupeEfforts(list []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(list))
	for _, v := range list {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func betterVirtualDisplay(nextDisplay, nextRaw, oldDisplay, oldRaw string) bool {
	nextBare := strings.EqualFold(nextRaw, nextDisplay)
	oldBare := strings.EqualFold(oldRaw, oldDisplay)
	if nextBare != oldBare {
		return nextBare
	}
	if len(nextDisplay) != len(oldDisplay) {
		return len(nextDisplay) < len(oldDisplay)
	}
	return nextDisplay < oldDisplay
}
func (p *panel) modelsFor(ctx context.Context, c core.Client, refresh bool) clientModels {
	row := clientModels{Name: c.Name(), Models: []modelInfo{}}
	effortProvider, hasEffortProvider := core.AsModelEfforts(c)

	refresher, canRefresh := core.AsModelRefresher(c)
	row.CanRefresh = canRefresh

	var (
		list []core.Model
		err  error
	)
	if refresh && canRefresh {
		// RefreshModels is documented to return its last good list alongside
		// the error, so the error must never blank the catalogue.
		list, err = refresher.RefreshModels(ctx)
	} else {
		list, err = c.Models(ctx)
	}

	if err != nil {
		row.Error = core.Redact(err.Error())
		if len(list) == 0 {
			// Showing nothing with no explanation is the worst outcome; say
			// that the list is empty and why.
			row.Error += " (no models returned)"
		}
	}

	for _, m := range list {
		extra, _ := core.RedactAny(m.Extra).(map[string]any)
		if extra == nil {
			extra = map[string]any{}
		}
		// The panel's 免费 chip follows the same rule the router uses, so a
		// model the vendor labels free by name or group reads as free here
		// too, not only when the module could read its price.
		if core.ModelIsFree(m) {
			extra["free"] = true
		}
		owned := m.OwnedBy
		if owned == "" {
			owned = c.Name()
		}
		vendor := modelmeta.FromExtra(extra, modelmeta.KeysCanonical, "")
		if hasEffortProvider {
			if efforts, defaultEffort, ok := effortProvider.ModelEfforts(ctx, m.ID); ok {
				vendor.Efforts = append([]string(nil), efforts...)
				vendor.DefaultEffort = defaultEffort
				if vendor.FieldSources == nil {
					vendor.FieldSources = map[string]string{}
				}
				vendor.FieldSources[modelmeta.FieldEfforts] = modelmeta.SourceVendor
				if defaultEffort != "" {
					vendor.FieldSources[modelmeta.FieldDefaultEffort] = modelmeta.SourceVendor
				} else {
					delete(vendor.FieldSources, modelmeta.FieldDefaultEffort)
				}
				vendor.Source = modelmeta.SourceVendor
			}
		}
		realm := modelRealmForID(m.ID)
		provider := modelmeta.New(modelmeta.Options{Client: c.Name(), Realm: realm, Overrides: p.opts.ModelOverrides})
		// plain resolves the same chain without the operator's overrides, so a row
		// can offer "restore default" and show what the value falls back to.
		plain := modelmeta.New(modelmeta.Options{Client: c.Name(), Realm: realm})
		meta := provider.Merge(m.ID, vendor)
		base := plain.Merge(m.ID, vendor)
		// The panel treats the client's published catalogue as the default
		// capability: it is what the user sees before touching the row, and it
		// must not be masked by an upstream Extra value from a different
		// catalogue. The manual override remains the final authority.
		if clientMeta, ok := modelmeta.StaticMeta(c.Name(), realm, m.ID); ok {
			meta = modelmeta.PreferLimits(meta, clientMeta)
			base = modelmeta.PreferLimits(base, clientMeta)
		}
		if p.opts.ModelOverrides != nil {
			if o, ok := p.opts.ModelOverrides.Get(c.Name(), m.ID); ok {
				meta = modelmeta.ApplyOverride(meta, o)
			}
		}
		// The effective policy is the shared resolver, so the checkbox reflects
		// what the gateway will actually do (an embedded default applies unless
		// the operator recorded an explicit choice).
		elevateTop := modelmeta.ElevateTop(c.Name(), m.ID, p.opts.ModelOverrides)
		elevateTopManual := false
		if p.opts.ModelOverrides != nil {
			if o, ok := p.opts.ModelOverrides.Get(c.Name(), m.ID); ok {
				_, elevateTopManual = o.ExplicitElevateTop()
			}
		}
		row.Models = append(row.Models, modelInfo{
			ID:                      core.Redact(m.ID),
			OwnedBy:                 core.Redact(owned),
			Extra:                   extra,
			ContextLength:           meta.ContextLength,
			MaxOutputTokens:         meta.MaxOutputTokens,
			ContextSource:           meta.SourceOf(modelmeta.FieldContextLength),
			MaxOutputSource:         meta.SourceOf(modelmeta.FieldMaxOutputTokens),
			SupportedEfforts:        append([]string(nil), meta.Efforts...),
			DefaultEffort:           meta.DefaultEffort,
			EffortsSource:           meta.SourceOf(modelmeta.FieldEfforts),
			DefaultEffortSource:     meta.SourceOf(modelmeta.FieldDefaultEffort),
			SupportedEffortsDefault: append([]string(nil), base.Efforts...),
			DefaultEffortDefault:    base.DefaultEffort,
			ElevateTop:              elevateTop,
			ElevateTopDefault:       modelmeta.StaticElevateTop(c.Name(), m.ID),
			ElevateTopManual:        elevateTopManual,
			InputPerMillion:         meta.InputPerMillion,
			OutputPerMillion:        meta.OutputPerMillion,
			CacheReadPerMillion:     meta.CacheReadPerMillion,
			HasPrice:                meta.HasPrice,
			HasCacheRead:            meta.HasCacheRead,
			InputPriceSource:        meta.SourceOf(modelmeta.FieldInputPrice),
			OutputPriceSource:       meta.SourceOf(modelmeta.FieldOutputPrice),
			CacheReadPriceSource:    meta.SourceOf(modelmeta.FieldCacheReadPrice),
			InputPriceDefault:       base.InputPerMillion,
			OutputPriceDefault:      base.OutputPerMillion,
			CacheReadPriceDefault:   base.CacheReadPerMillion,
			DefaultHasPrice:         base.HasPrice,
			DefaultHasCacheRead:     base.HasCacheRead,
			PriceEditable:           p.opts.ModelOverrides != nil,
			ContextDefault:          base.ContextLength,
			MaxOutputDefault:        base.MaxOutputTokens,
			ContextEditable:         p.opts.ModelOverrides != nil,
			MaxOutputEditable:       p.opts.ModelOverrides != nil,
			EffortsEditable:         p.opts.ModelOverrides != nil,
			ElevateTopEditable:      p.opts.ModelOverrides != nil,
		})
	}
	return row
}

func modelRealmForID(id string) string {
	if prefix, rest, ok := strings.Cut(strings.TrimSpace(id), ":"); ok && rest != "" && prefix == modelmeta.RealmCN {
		return modelmeta.RealmCN
	}
	if prefix, rest, ok := strings.Cut(strings.TrimSpace(id), ":"); ok && rest != "" && prefix == modelmeta.RealmGlobal {
		return modelmeta.RealmGlobal
	}
	return modelmeta.RealmCN
}
