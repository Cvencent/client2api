package workbuddy

import (
	"strings"

	"client2api/internal/core"
	"client2api/internal/prompt"
)

// The three system-prompt strategies, spelled exactly as the reference's
// configuration does.  They live here rather than in internal/prompt because
// that package deliberately knows nothing about which strategy the operator
// picked: it only knows how to carry one out.
const (
	promptModePassthrough = "passthrough"
	promptModeCustom      = "custom"
	promptModeAppend      = "append"
)

// degradeOption is the per-request marker Degrade leaves in a chat request.
//
// It rides in Options rather than on the Client because the gateway retries the
// very same request object after it reports a content block, while other
// requests are in flight: a flag on the Client would degrade every conversation
// at once, and it would also make "this one request was degraded" impossible to
// assert in an offline test.
const degradeOption = "prompt_degraded"

// normalizePromptMode maps a configured mode onto one of the three constants.
// The empty string is passthrough -- that is the reference's default -- while
// anything unrecognised is reported to the caller so the operator hears about
// the typo instead of silently getting a strategy they did not ask for.
func normalizePromptMode(mode string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", promptModePassthrough:
		return promptModePassthrough, true
	case promptModeCustom:
		return promptModeCustom, true
	case promptModeAppend:
		return promptModeAppend, true
	default:
		return "", false
	}
}

// logf is a nil-safe module log: deps.Log is a nil-safe convenience wrapper
// around the optional deps.Logf, so no guard is needed here (and go vet
// rejects one, because deps.Log is a method value and never nil).
func (c *Client) logf(format string, args ...any) {
	c.deps.Log(format, args...)
}

func (c *Client) setPromptState(mode, text string) {
	c.promptMu.Lock()
	c.promptMode, c.promptText = mode, text
	c.promptMu.Unlock()
}

func (c *Client) promptState() (string, string) {
	c.promptMu.RLock()
	defer c.promptMu.RUnlock()
	return c.promptMode, c.promptText
}

// ApplyLive implements core.LiveReloader.
//
// It must be safe to call concurrently with in-flight requests, and an empty
// field means "the file did not mention this", so a partial reload never clears
// a value the operator did not touch.
func (c *Client) ApplyLive(s core.LiveSettings) {
	if s.SanitizeFingerprints != nil {
		c.up.setSanitize(*s.SanitizeFingerprints)
	}
	// The pool ceilings travel with the same push, and they are applied before
	// the prompt branch's early return: they have nothing to do with the
	// prompt, so "prompt mode was not mentioned" must not drop them.
	if s.MaxInFlight != nil {
		c.pool.SetMaxInFlight(*s.MaxInFlight)
	}
	if s.MaxInFlightGlobal != nil {
		c.pool.SetMaxInFlightGlobal(*s.MaxInFlightGlobal)
	}
	if s.MaxInFlight != nil || s.MaxInFlightGlobal != nil {
		per, global := c.pool.Limits()
		c.logf("workbuddy: in-flight ceiling is %d per account, %d on the global realm (0 = no ceiling / not set)", per, global)
	}
	// session_sticky.* travels with the same push, and like the ceilings it is
	// applied before the prompt branch's early return: how long a conversation
	// stays on one account has nothing to do with the system prompt.
	//
	// The switch and the window are different knobs.  A false is not a window
	// of zero: it removes the routing step, exactly as the reference does when
	// it builds no session router at all.
	if s.AffinityEnabled != nil {
		c.affinity.SetEnabled(*s.AffinityEnabled)
	}
	if s.AffinityTTL > 0 {
		c.affinity.SetTTL(s.AffinityTTL)
	}
	if s.AffinityGCInterval > 0 {
		c.affinity.SetGCInterval(s.AffinityGCInterval)
	}
	if s.AffinityEnabled != nil || s.AffinityTTL > 0 || s.AffinityGCInterval > 0 {
		c.logf("workbuddy: conversation stickiness enabled=%t, window %s, swept every %s",
			c.affinity.Enabled(), c.affinity.TTL(), c.affinity.GCInterval())
	}
	// pool.* travels with the same push, and again before the prompt branch's
	// early return: how the router punishes failures has nothing to do with the
	// system prompt.  These keys used to be parsed, defaulted and logged with
	// no reader at all, so the breaker, the degradation park, the idle curve
	// and the expiring-credit preference were all silently at their constants.
	c.applyPoolTuning(s.Pool)
	if strings.TrimSpace(s.PromptMode) == "" {
		return
	}
	mode, ok := normalizePromptMode(s.PromptMode)
	if !ok {
		cur, _ := c.promptState()
		c.logf("workbuddy: unknown prompt mode %q, keeping %q", s.PromptMode, cur)
		return
	}
	if mode == promptModePassthrough {
		c.setPromptState(mode, "")
		return
	}
	text, err := prompt.Load(mode, s.PromptFile)
	if err != nil {
		// Falling back is the only safe answer: a replacement prompt that
		// cannot be read would otherwise fail every request, whereas
		// passthrough keeps working.
		c.logf("workbuddy: prompt %s could not be loaded (%v); passing the client's own system prompt through", mode, err)
		c.setPromptState(promptModePassthrough, "")
		return
	}
	if strings.TrimSpace(text) == "" {
		c.logf("workbuddy: prompt %s is empty; passing the client's own system prompt through", mode)
		c.setPromptState(promptModePassthrough, "")
		return
	}
	c.setPromptState(mode, text)
	c.logf("workbuddy: system prompt strategy is now %s", mode)
}

// Degrade implements core.Degrader: it marks this one request for the minimal
// system prompt and reports whether the gateway should spend a retry on it.
func (c *Client) Degrade(req *core.ChatRequest) bool {
	if req == nil {
		return false
	}
	mode, _ := c.promptState()
	// Only the two strategies that carry the client's own system text forward
	// have anything to rescue.  A custom prompt was chosen deliberately, so the
	// gateway must not spend its one retry replacing it with the built-in
	// fallback the operator did not pick.
	if mode != promptModePassthrough && mode != promptModeAppend {
		return false
	}
	// The window is already open, which means this request was served the
	// minimal prompt on its first attempt; rewriting again would send the same
	// body and hit the same wall.
	if c.deps.Guard.ContentBlocked() {
		return false
	}
	if req.Options == nil {
		req.Options = make(map[string]any)
	}
	req.Options[degradeOption] = true
	return true
}

// degradeRequested reports whether Degrade marked this request, which is how
// the retry is served the minimal prompt even if the gateway's degraded window
// is somehow not open yet.
func degradeRequested(req *core.ChatRequest) bool {
	if req == nil || req.Options == nil {
		return false
	}
	v, _ := req.Options[degradeOption].(bool)
	return v
}

// applyPrompt runs the system-prompt stage.  It runs exactly once per request,
// before the rotation loop, because Append is not idempotent: running it per
// attempt would stack a second copy of the operator's prompt on the first.
//
// The stage order is the reference's -- rewrite the prompt, then normalise the
// payload (body.go's prepareBody, whose last step is the sanitizer).
func (c *Client) applyPrompt(body []byte, degraded bool) []byte {
	if len(body) == 0 {
		return body
	}
	mode, text := c.promptState()
	if degraded || c.deps.Guard.ContentBlocked() {
		// The degraded window is sticky until the next 00:00 CST, exactly as in
		// the reference: every request in the window is rewritten, not only the
		// one that opened it.  Append collapses to replace here on purpose --
		// retrying with the client's own system text is a deterministic way to
		// be refused again.
		if mode == promptModePassthrough || mode == promptModeAppend {
			return prompt.Rewrite(body, prompt.Degraded)
		}
	}
	switch mode {
	case promptModeCustom:
		if text != "" {
			return prompt.Rewrite(body, text)
		}
	case promptModeAppend:
		if text != "" {
			return prompt.Append(body, text)
		}
	}
	return body
}
