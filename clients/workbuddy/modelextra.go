package workbuddy

import "strings"

// creditsPrefix renders the upstream credits string the way the reference panel
// does.  The upstream is not consistent: the same field arrives as "x0.05
// credits", "x0.29" and "x0.00 credits", so the suffix is trimmed before the
// bracket is added.  An empty value — or one that is nothing but the suffix —
// yields "", which callers read as "no multiplier to show".
func creditsPrefix(raw string) string {
	s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "credits"))
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// modelExtra flattens one upstream ModelInfo into the extra map that
// /panel/api/models and /v1/models serve.  The key names follow the reference
// panel, so the two front-ends read the same catalogue.
//
// Two rules are deliberate:
//
//   - Empty values are omitted.  "The upstream did not say" and "the upstream
//     said zero" are different facts, and a zero renders as a real limit.
//   - The older spellings this module already served (context_window,
//     max_tokens, reasoning_efforts) are kept alongside the reference ones
//     (context_length, max_output_tokens, reasoning_supported_efforts) so that
//     no existing reader loses a key.
func modelExtra(mi ModelInfo) map[string]any {
	extra := map[string]any{}

	setStr := func(key, val string) {
		if v := strings.TrimSpace(val); v != "" {
			extra[key] = v
		}
	}
	setFlag := func(key string, on bool) {
		if on {
			extra[key] = true
		}
	}
	setInt := func(key string, val int64) {
		if val > 0 {
			extra[key] = val
		}
	}
	setList := func(key string, val []string) {
		if len(val) > 0 {
			extra[key] = append([]string(nil), val...)
		}
	}

	setStr("name", mi.Name)

	// The multiplier is prefixed onto the description exactly as the reference
	// does, so a consumer that renders only `description` still learns what the
	// model costs.  A model with no description still gets the prefix alone
	// rather than losing the multiplier.
	desc := strings.TrimSpace(mi.Description)
	if p := creditsPrefix(mi.Credits); p != "" {
		if desc == "" {
			desc = p
		} else {
			desc = p + " " + desc
		}
	}
	setStr("description", desc)

	// The list price and the currently effective promotion.  They are separate
	// facts: `credits` is what the model costs once the trial is over, and the
	// promo_* trio is the limited-time discount that is in force right now.
	setStr("credits", mi.Credits)
	if mi.PromoFactor != nil {
		extra["promo_factor"] = *mi.PromoFactor
	}
	setStr("promo_credits", mi.PromoCredits)
	setStr("promo_label", mi.PromoLabel)
	setStr("promo_note", mi.PromoNote)

	setList("tags", mi.Tags)
	setStr("vendor", mi.Vendor)

	setFlag("is_default", mi.IsDefault)
	setFlag("supports_reasoning", mi.SupportsReasoning)
	setFlag("can_disable_thinking", mi.CanDisableThinking)
	setFlag("supports_tool_call", mi.SupportsToolCall)
	setFlag("only_reasoning", mi.OnlyReasoning)
	setFlag("supports_images", mi.SupportsImages)
	setInt("max_allowed_size", mi.MaxAllowedSize)

	setStr("reasoning_effort", mi.ReasoningEffort)
	setStr("reasoning_summary", mi.ReasoningSummary)

	setInt("context_length", mi.ContextWindow)
	setInt("max_output_tokens", mi.MaxTokens)
	setList("reasoning_supported_efforts", mi.Efforts)
	setStr("reasoning_default_effort", mi.DefaultEffort)

	// The spellings this module served before the reference keys were added.
	setInt("context_window", mi.ContextWindow)
	setInt("max_tokens", mi.MaxTokens)
	setList("reasoning_efforts", mi.Efforts)

	return extra
}
