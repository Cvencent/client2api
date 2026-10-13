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
// and then applies cap alignment. Resetting matters on failover: an earlier
// candidate may have promoted xhigh to max, while the next candidate may not
// support max and must start from the original request again.
func (s *server) applyEffortPolicy(ctx context.Context, client core.Client, model string, req *core.ChatRequest, requested string) string {
	if req == nil || requested == "" {
		return ""
	}
	requested = strings.ToLower(strings.TrimSpace(requested))
	setReasoningEffort(req, requested)
	req.EffortFinal = false
	if requested != effortXHigh || client == nil {
		return requested
	}
	// Cap alignment is a per-model policy with an embedded default: an operator
	// may switch it off explicitly, but a model whose table opts in stays on even
	// with no override store wired at all.
	if !modelmeta.ElevateTop(client.Name(), model, s.opts.ModelOverrides) {
		return requested
	}
	efforts, _, ok := s.resolveModelEfforts(ctx, client, model)
	if !ok {
		return requested
	}
	top := highestReasoningEffort(efforts)
	if top == "" || reasoningEffortRank[top] <= reasoningEffortRank[requested] {
		return requested
	}
	setReasoningEffort(req, top)
	req.EffortFinal = true
	return top
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
