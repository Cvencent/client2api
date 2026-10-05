package gateway

import (
	"context"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

// resolveMaxOutputTokens applies the same precedence as the model panel:
// operator override, live module capability, then the embedded official table.
func (s *server) resolveMaxOutputTokens(ctx context.Context, client core.Client, model string) (int, bool) {
	if s.opts.ModelOverrides != nil {
		if o, ok := s.opts.ModelOverrides.Get(client.Name(), model); ok && o.MaxOutputTokens > 0 {
			return int(o.MaxOutputTokens), true
		}
	}
	if limits, ok := core.AsModelLimits(client); ok {
		if n, ok := limits.ModelMaxOutputTokens(ctx, model); ok && n > 0 {
			return n, true
		}
	}
	if m, ok := modelmeta.StaticMeta(client.Name(), modelmeta.RealmCN, model); ok && m.MaxOutputTokens > 0 {
		return int(m.MaxOutputTokens), true
	}
	return 0, false
}

// resolvedModelMeta resolves a model's context window and output cap from the
// layers a catalogue listing can see without touching the network: the
// operator's manual override, the vendor Extra the module already returned, and
// the embedded official table. It mirrors the panel's resolution exactly so
// /v1/models and /panel/api/models never disagree about the same model.
//
// This is what makes a downstream client (Codex, Claude Code, any OpenAI-
// compatible caller) size its context correctly instead of falling back to a
// tiny built-in default when the vendor reported nothing.
func (s *server) resolvedModelMeta(client core.Client, model string, extra map[string]any) modelmeta.Meta {
	provider := modelmeta.New(modelmeta.Options{Client: client.Name(), Overrides: s.opts.ModelOverrides})
	vendor := modelmeta.FromExtra(extra, modelmeta.KeysCanonical, "")
	return provider.Merge(model, vendor)
}
