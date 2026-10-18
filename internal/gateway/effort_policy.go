package gateway

import (
	"context"
	"strings"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

const effortXHigh = "xhigh"

// reasoningEffortRank orders the vocabulary the gateway accepts. Unknown
// labels are ignored for promotion, because guessing where a vendor's custom
// tier sits would be less safe than leaving the caller's value alone.
var reasoningEffortRank = map[string]int{
	"off":     0,
	"minimal": 1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

// applyEffortPolicy resets a candidate request to the caller's requested level
// and then applies the configured effort mappings. Resetting matters on failover: an earlier
// candidate may have promoted xhigh to max, while the next candidate may not
// support max and must start from the original request again.
func (s *server) applyEffortPolicy(ctx context.Context, client core.Client, model, requestedModel string, req *core.ChatRequest, requested string) string {
	if req == nil {
		return ""
	}
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" {
		return ""
	}
	setReasoningEffort(req, requested)
	req.EffortFinal = false
	if client == nil {
		return requested
	}
	efforts, _, _ := s.resolveModelEfforts(ctx, client, model)
	custom, defaults := s.effortMappingsFor(requestedModel, client, model)
	// An operator-defined rule is authoritative. It may name a tier the
	// candidate does not publish, and it is applied even before the candidate's
	// ladder is known, because the operator explicitly asked for that value.
	// This is what lets max -> high and max -> max work on a model that only
	// advertises high.
	for _, mapping := range custom {
		if normalizeEffort(mapping.From) != requested {
			continue
		}
		target := normalizeEffort(mapping.To)
		if target == modelmeta.EffortMappingTop {
			if target = highestReasoningEffort(efforts); target == "" {
				continue
			}
		}
		if target == "" {
			continue
		}
		setReasoningEffort(req, target)
		req.EffortFinal = true
		return target
	}
	if len(efforts) == 0 {
		return requested
	}
	// The built-in xhigh -> top rule stays conservative: it only moves upward
	// and only to a tier the candidate actually published. A global-only model
	// that advertises just high therefore keeps Codex's xhigh unchanged instead
	// of being downgraded.
	for _, mapping := range defaults {
		if normalizeEffort(mapping.From) != requested || normalizeEffort(mapping.To) != modelmeta.EffortMappingTop {
			continue
		}
		target := highestReasoningEffort(efforts)
		targetRank, targetKnown := reasoningEffortRank[target]
		requestedRank, requestedKnown := reasoningEffortRank[requested]
		if !targetKnown || !requestedKnown || targetRank <= requestedRank {
			break
		}
		setReasoningEffort(req, target)
		req.EffortFinal = true
		return target
	}
	// A caller's top tier must not be silently downgraded merely because the
	// candidate's published vocabulary stops at high.  Downward alignment is
	// useful for unsupported middle tiers, but xhigh is the Codex top label and
	// the existing contract leaves it untouched unless a higher vendor tier is
	// available (handled by the mapping rules above).
	if requested == effortXHigh {
		return requested
	}
	if !containsEffort(efforts, requested) {
		if aligned, changed := alignEffortToSupported(requested, efforts); changed {
			setReasoningEffort(req, aligned)
			req.EffortFinal = true
			return aligned
		}
	}
	return requested
}

// effortMappingsFor resolves the mapping policy for the model the caller
// actually named. Custom rules are returned separately from the built-in
// xhigh -> top default because they have different authority: a custom rule is
// final, while the built-in rule is only an upward cap alignment. A virtual
// row's explicit enable/disable or custom rules are final for every underlying
// candidate; without one, each candidate keeps its own model-level rules.
func (s *server) effortMappingsFor(requestedModel string, client core.Client, model string) (custom, defaults []modelmeta.EffortMapping) {
	if name, ok := s.virtualModelName(requestedModel); ok && s.opts.ModelOverrides != nil {
		if o, ok := s.opts.ModelOverrides.Get(core.VirtualModelClient, name); ok {
			if on, explicit := o.ExplicitElevateTop(); explicit {
				if !on {
					return nil, nil
				}
				return append([]modelmeta.EffortMapping(nil), o.EffortMappings...), modelmeta.DefaultEffortMappings()
			}
			if len(o.EffortMappings) > 0 {
				return append([]modelmeta.EffortMapping(nil), o.EffortMappings...), modelmeta.DefaultEffortMappings()
			}
		}
	}
	if !modelmeta.ElevateTop(client.Name(), model, s.opts.ModelOverrides) {
		return nil, nil
	}
	if s.opts.ModelOverrides != nil {
		if o, ok := s.opts.ModelOverrides.Get(client.Name(), model); ok && len(o.EffortMappings) > 0 {
			return append([]modelmeta.EffortMapping(nil), o.EffortMappings...), modelmeta.DefaultEffortMappings()
		}
	}
	return nil, modelmeta.DefaultEffortMappings()
}

// alignEffortToSupported maps a caller's unsupported tier onto the candidate's
// nearest supported tier without changing candidate order. It prefers the
// closest lower rung and only promotes when the candidate publishes nothing at
// or below the requested level.
func alignEffortToSupported(requested string, efforts []string) (string, bool) {
	wantRank, known := reasoningEffortRank[requested]
	if !known {
		return requested, false
	}
	bestLower, bestLowerRank := "", -1
	bestHigher, bestHigherRank := "", 0
	for _, raw := range efforts {
		effort := strings.ToLower(strings.TrimSpace(raw))
		rank, ok := reasoningEffortRank[effort]
		if !ok {
			continue
		}
		if rank <= wantRank && rank > bestLowerRank {
			bestLower, bestLowerRank = effort, rank
		}
		if rank > wantRank && (bestHigher == "" || rank < bestHigherRank) {
			bestHigher, bestHigherRank = effort, rank
		}
	}
	if bestLower != "" {
		return bestLower, bestLower != requested
	}
	if bestHigher != "" {
		return bestHigher, true
	}
	return requested, false
}

func setReasoningEffort(req *core.ChatRequest, effort string) {
	if req == nil || effort == "" {
		return
	}
	if req.Options == nil {
		req.Options = map[string]any{}
	}
	req.Options["reasoning_effort"] = effort
}

// optionEffort reads the reasoning level already carried by a request.  The
// virtual-model path injects a default there before the candidate is prepared;
// treating it as the target level lets each candidate align that default to its
// own ladder while the usage row keeps the caller's original empty value.
func optionEffort(options map[string]any) string {
	if len(options) == 0 {
		return ""
	}
	switch value := options["reasoning_effort"].(type) {
	case string:
		return normalizeEffort(value)
	default:
		return ""
	}
}

func highestReasoningEffort(efforts []string) string {
	best := ""
	bestRank := -1
	for _, raw := range efforts {
		effort := strings.ToLower(strings.TrimSpace(raw))
		rank, ok := reasoningEffortRank[effort]
		if !ok || rank <= bestRank {
			continue
		}
		best, bestRank = effort, rank
	}
	return best
}

// resolveModelEfforts applies the same precedence as the model panel:
// operator override, the module's cached facts, then the embedded table.
func (s *server) resolveModelEfforts(ctx context.Context, client core.Client, model string) ([]string, string, bool) {
	var (
		manualEfforts []string
		manualDefault string
	)
	if s.opts.ModelOverrides != nil && client != nil {
		if o, ok := s.opts.ModelOverrides.Get(client.Name(), model); ok {
			manualEfforts = append([]string(nil), o.SupportedEfforts...)
			manualDefault = o.DefaultEffort
		}
	}
	if client != nil {
		if provider, ok := core.AsModelEfforts(client); ok {
			efforts, def, found := provider.ModelEfforts(ctx, model)
			if found || len(manualEfforts) > 0 {
				if len(manualEfforts) > 0 {
					efforts = manualEfforts
				}
				if manualDefault != "" {
					def = manualDefault
				}
				return efforts, def, len(efforts) > 0
			}
		}
	}
	if len(manualEfforts) > 0 {
		return manualEfforts, manualDefault, true
	}
	if client != nil {
		if efforts, def, ok := staticModelEfforts(client.Name(), model); ok {
			if manualDefault != "" {
				def = manualDefault
			}
			return efforts, def, true
		}
	}
	return nil, manualDefault, false
}

// virtualEffortFor returns the default thinking level a virtual model should
// inject when the caller omitted one.  A manual __virtual__ override wins;
// otherwise the first real candidate in routing order supplies its advertised
// default.  Returning "" leaves the request untouched, so ordinary models
// retain their existing "let the module decide" behaviour.
func (s *server) virtualEffortFor(requested string, candidates []core.Candidate) string {
	name, ok := s.virtualModelName(requested)
	if !ok || len(candidates) == 0 {
		return ""
	}
	if s.opts.ModelOverrides != nil {
		if o, ok := s.opts.ModelOverrides.Get(core.VirtualModelClient, name); ok {
			if def := normalizeEffort(o.DefaultEffort); def != "" {
				return def
			}
		}
	}
	first := candidates[0]
	if first.Client == nil {
		return ""
	}
	_, def, ok := s.resolveModelEfforts(context.Background(), first.Client, first.Model)
	if !ok {
		return ""
	}
	return normalizeEffort(def)
}

// virtualModelName reports whether a request named a gateway-level virtual
// model and returns the key the override store uses.  Auto/<id> is virtual by
// construction; a bare model-group name is virtual only while the registry
// still declares that exact group.
func (s *server) virtualModelName(requested string) (string, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", false
	}
	if len(requested) >= len("auto/") && strings.EqualFold(requested[:len("auto/")], "auto/") {
		return requested, strings.TrimSpace(requested[len("auto/"):]) != ""
	}
	if s.opts.Registry == nil {
		return "", false
	}
	groups := s.opts.Registry.ModelGroups()
	for name := range groups {
		if strings.EqualFold(strings.TrimSpace(name), requested) {
			return strings.TrimSpace(name), true
		}
	}
	return "", false
}

func staticModelEfforts(client, model string) ([]string, string, bool) {
	model = strings.TrimSpace(model)
	if m, ok := modelmeta.StaticMeta(client, modelmeta.RealmCN, model); ok && len(m.Efforts) > 0 {
		return append([]string(nil), m.Efforts...), m.DefaultEffort, true
	}
	realm := modelmeta.RealmCN
	bare := model
	if prefix, rest, ok := strings.Cut(model, ":"); ok && rest != "" {
		switch prefix {
		case modelmeta.RealmGlobal:
			realm, bare = modelmeta.RealmGlobal, rest
		case modelmeta.RealmCN:
			realm, bare = modelmeta.RealmCN, rest
		}
	}
	if bare == model {
		return nil, "", false
	}
	if m, ok := modelmeta.StaticMeta(client, realm, bare); ok && len(m.Efforts) > 0 {
		return append([]string(nil), m.Efforts...), m.DefaultEffort, true
	}
	return nil, "", false
}
