package gateway

import (
	"context"
	"strings"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

// resolveMaxOutputTokens applies the same precedence as the model panel:
// operator override, the client-published preset, then live module capability.
func (s *server) resolveMaxOutputTokens(ctx context.Context, client core.Client, model string) (int, bool) {
	if s.opts.ModelOverrides != nil {
		if o, ok := s.opts.ModelOverrides.Get(client.Name(), model); ok && o.MaxOutputTokens > 0 {
			return int(o.MaxOutputTokens), true
		}
	}
	realm := modelmeta.RealmFromModel(model)
	if m, ok := modelmeta.StaticMeta(client.Name(), realm, model); ok && m.MaxOutputTokens > 0 {
		return int(m.MaxOutputTokens), true
	}
	if bare := trimRealmPrefix(model); bare != model {
		if m, ok := modelmeta.StaticMeta(client.Name(), realm, bare); ok && m.MaxOutputTokens > 0 {
			return int(m.MaxOutputTokens), true
		}
	}
	if limits, ok := core.AsModelLimits(client); ok {
		if n, ok := limits.ModelMaxOutputTokens(ctx, model); ok && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// resolvedModelMeta resolves a model's context window and output cap from the
// layers a catalogue listing can see without touching the network: the
// operator's manual override, the client-published preset, and the vendor Extra
// the module already returned. It mirrors the panel's resolution exactly so
// /v1/models and /panel/api/models never disagree about the same model.
//
// This is what makes a downstream client (Codex, Claude Code, any OpenAI-
// compatible caller) size its context correctly instead of falling back to a
// tiny built-in default when the vendor reported nothing.
func (s *server) resolvedModelMeta(client core.Client, model string, extra map[string]any) modelmeta.Meta {
	provider := modelmeta.New(modelmeta.Options{Client: client.Name(), Realm: modelmeta.RealmFromModel(model), Overrides: s.opts.ModelOverrides})
	vendor := modelmeta.FromExtra(extra, modelmeta.KeysCanonical, "")
	meta := provider.Merge(model, vendor)
	if preset, ok := modelmeta.StaticMeta(client.Name(), modelmeta.RealmFromModel(model), model); ok {
		meta = modelmeta.PreferLimits(meta, preset)
	}
	if bare := trimRealmPrefix(model); bare != model {
		if preset, ok := modelmeta.StaticMeta(client.Name(), modelmeta.RealmFromModel(model), bare); ok {
			meta = modelmeta.PreferLimits(meta, preset)
		}
	}
	if s.opts.ModelOverrides != nil {
		if o, ok := s.opts.ModelOverrides.Get(client.Name(), model); ok {
			meta = modelmeta.ApplyOverride(meta, o)
		}
	}
	return meta
}

// resolvedModelMetaWithEfforts is resolvedModelMeta plus the optional live
// effort capability.  Context/output already travel in the catalogue Extra,
// but several modules expose the thinking ladder only through ModelEfforts;
// reading it here keeps /v1/models, Auto aggregation and model groups in sync
// with request-time routing without making the catalogue path fetch anything.
func (s *server) resolvedModelMetaWithEfforts(ctx context.Context, client core.Client, model string, extra map[string]any) modelmeta.Meta {
	meta := s.resolvedModelMeta(client, model, extra)
	if client == nil {
		return meta
	}
	provider, ok := core.AsModelEfforts(client)
	if !ok {
		return meta
	}
	efforts, defaultEffort, found := provider.ModelEfforts(ctx, model)
	if !found || (len(efforts) == 0 && defaultEffort == "") {
		return meta
	}
	if meta.FieldSources == nil {
		meta.FieldSources = map[string]string{}
	}
	// A manual vocabulary is the operator's final answer.  A live default may
	// still fill the empty default slot, but only when the chosen vocabulary
	// actually contains it.
	if meta.SourceOf(modelmeta.FieldEfforts) != modelmeta.SourceManual {
		meta.Efforts = dedupeEfforts(efforts)
		if len(meta.Efforts) > 0 {
			meta.FieldSources[modelmeta.FieldEfforts] = modelmeta.SourceVendor
		} else {
			delete(meta.FieldSources, modelmeta.FieldEfforts)
		}
	}
	if meta.SourceOf(modelmeta.FieldDefaultEffort) != modelmeta.SourceManual {
		defaultEffort = strings.ToLower(strings.TrimSpace(defaultEffort))
		if defaultEffort != "" && containsEffort(meta.Efforts, defaultEffort) {
			meta.DefaultEffort = defaultEffort
			meta.FieldSources[modelmeta.FieldDefaultEffort] = modelmeta.SourceVendor
		} else {
			meta.DefaultEffort = ""
			delete(meta.FieldSources, modelmeta.FieldDefaultEffort)
		}
	}
	return meta
}

// virtualModelMeta resolves metadata for a gateway-level virtual model
// (Auto/<model> or a model-group name).  The operator's manual override is the
// final authority; without one, the fallback is the largest context window and
// output cap any of the underlying candidates publishes, so a caller reading
// /v1/models can size its context for the biggest platform the name may route
// to.  Routing still trims per candidate at request time.
func (s *server) virtualModelMeta(id string, fallback modelmeta.Meta) modelmeta.Meta {
	meta := fallback
	if meta.FieldSources == nil {
		meta.FieldSources = map[string]string{}
	}
	if meta.ContextLength > 0 {
		meta.FieldSources[modelmeta.FieldContextLength] = modelmeta.SourceVendor
	}
	if meta.MaxOutputTokens > 0 {
		meta.FieldSources[modelmeta.FieldMaxOutputTokens] = modelmeta.SourceVendor
	}
	if s.opts.ModelOverrides != nil {
		if o, ok := s.opts.ModelOverrides.Get(core.VirtualModelClient, id); ok {
			if o.ContextLength > 0 {
				meta.ContextLength = o.ContextLength
				meta.FieldSources[modelmeta.FieldContextLength] = modelmeta.SourceManual
			}
			if o.MaxOutputTokens > 0 {
				meta.MaxOutputTokens = o.MaxOutputTokens
				meta.FieldSources[modelmeta.FieldMaxOutputTokens] = modelmeta.SourceManual
			}
			if len(o.SupportedEfforts) > 0 {
				meta.Efforts = append([]string(nil), o.SupportedEfforts...)
				meta.FieldSources[modelmeta.FieldEfforts] = modelmeta.SourceManual
			}
			if o.DefaultEffort != "" && containsEffort(meta.Efforts, o.DefaultEffort) {
				meta.DefaultEffort = o.DefaultEffort
				meta.FieldSources[modelmeta.FieldDefaultEffort] = modelmeta.SourceManual
			}
		}
	}
	meta.Efforts = dedupeEfforts(meta.Efforts)
	if meta.DefaultEffort != "" && !containsEffort(meta.Efforts, meta.DefaultEffort) {
		meta.DefaultEffort = ""
		delete(meta.FieldSources, modelmeta.FieldDefaultEffort)
	}
	if len(meta.Efforts) > 0 {
		if meta.FieldSources[modelmeta.FieldEfforts] == "" {
			meta.FieldSources[modelmeta.FieldEfforts] = modelmeta.SourceVendor
		}
	} else {
		delete(meta.FieldSources, modelmeta.FieldEfforts)
		delete(meta.FieldSources, modelmeta.FieldDefaultEffort)
	}
	return meta
}

// mergeWindow keeps the largest known window/output and the stable union of
// effort levels across the candidates a virtual model may route to.
func mergeWindow(dst modelmeta.Meta, src modelmeta.Meta) modelmeta.Meta {
	if src.ContextLength > dst.ContextLength {
		dst.ContextLength = src.ContextLength
	}
	if src.MaxOutputTokens > dst.MaxOutputTokens {
		dst.MaxOutputTokens = src.MaxOutputTokens
	}
	dst.Efforts = append(dst.Efforts, src.Efforts...)
	dst.Efforts = dedupeEfforts(dst.Efforts)
	return dst
}

func containsEffort(efforts []string, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	for _, effort := range efforts {
		if strings.ToLower(strings.TrimSpace(effort)) == want {
			return true
		}
	}
	return false
}

func dedupeEfforts(efforts []string) []string {
	seen := make(map[string]struct{}, len(efforts))
	out := make([]string, 0, len(efforts))
	for _, effort := range efforts {
		effort = strings.ToLower(strings.TrimSpace(effort))
		if effort == "" {
			continue
		}
		if _, ok := seen[effort]; ok {
			continue
		}
		seen[effort] = struct{}{}
		out = append(out, effort)
	}
	return out
}

func trimRealmPrefix(model string) string {
	model = strings.TrimSpace(model)
	for _, prefix := range []string{modelmeta.RealmCN + ":", modelmeta.RealmGlobal + ":"} {
		if strings.HasPrefix(strings.ToLower(model), prefix) {
			return strings.TrimSpace(model[len(prefix):])
		}
	}
	return model
}
