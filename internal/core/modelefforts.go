package core

import (
	"context"
	"strings"
)

// ModelEffortProvider is the optional capability modules implement when their
// cached catalogue knows a model's reasoning-effort vocabulary.
//
// The gateway owns effort policy: this interface only reports facts, and a
// module that cannot answer stays exactly as it was. The contract mirrors
// ModelLimitsProvider: the lookup must be cheap and side-effect free, because
// it runs in the request path and must never trigger a catalogue fetch.
type ModelEffortProvider interface {
	Client
	// ModelEfforts reports the accepted effort levels and the advertised
	// default for the resolved upstream id. ok=false means "this module cannot
	// say"; the gateway then falls back to the embedded table or leaves the
	// caller's value unchanged.
	ModelEfforts(ctx context.Context, model string) (efforts []string, defaultEffort string, ok bool)
}

// AsModelEfforts narrows a registered client.
func AsModelEfforts(c Client) (ModelEffortProvider, bool) {
	p, ok := c.(ModelEffortProvider)
	return p, ok
}

// ModelEffortsFor finds a model in a cached catalogue and reads the effort
// vocabulary modules already project into Extra. Both the canonical keys and
// the older WorkBuddy spellings are accepted because the catalogue is served
// directly to existing panel builds as well.
func ModelEffortsFor(models []Model, id string) ([]string, string, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, "", false
	}
	for _, m := range models {
		if strings.TrimSpace(m.ID) != id {
			continue
		}
		efforts := extraEfforts(m.Extra)
		if len(efforts) == 0 {
			continue
		}
		def := extraEffortDefault(m.Extra)
		if def != "" && !effortInList(efforts, def) {
			def = ""
		}
		return efforts, def, true
	}
	return nil, "", false
}

func extraEfforts(extra map[string]any) []string {
	if len(extra) == 0 {
		return nil
	}
	for _, key := range []string{"supported_efforts", "reasoning_supported_efforts", "reasoning_efforts"} {
		if values := stringList(extra[key]); len(values) > 0 {
			return normalizeEffortList(values)
		}
	}
	return nil
}

func extraEffortDefault(extra map[string]any) string {
	if len(extra) == 0 {
		return ""
	}
	for _, key := range []string{"default_effort", "reasoning_default_effort"} {
		if value, ok := extra[key].(string); ok {
			if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
				return value
			}
		}
	}
	return ""
}

func normalizeEffortList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func stringList(value any) []string {
	switch list := value.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			text, ok := item.(string)
			if !ok {
				return nil
			}
			out = append(out, text)
		}
		return out
	default:
		return nil
	}
}

func effortInList(list []string, want string) bool {
	for _, value := range list {
		if value == want {
			return true
		}
	}
	return false
}
