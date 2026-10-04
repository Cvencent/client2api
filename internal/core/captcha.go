package core

import (
	"context"
	"strings"
)

// A captcha is the one thing a headless server cannot do for itself.  Several
// vendors gate a reward behind a token that only their own JavaScript can mint:
// the browser loads the vendor's SDK, the SDK fingerprints the client and
// returns an opaque string, and the reward endpoint wants that string in a
// header.  client2api has no browser, so the flow is split in two:
//
//   - the panel (which IS a browser) runs the SDK and hands back the string;
//   - the module (which knows the vendor's protocol) says what the panel should
//     run, and accepts the string when it arrives.
//
// CaptchaProvider is the first half of that contract.  The second half is
// carried in the context, because the string is per-CALL, not per-client: see
// CaptchaSolution.
//
// Both halves are optional.  A module that never hits a captcha implements
// neither, keeps its old signature, and its panel button behaves exactly as it
// did before.
//
// CaptchaScene describes what a browser would have to run before an action can
// be attempted.
//
// The fields are the vendor's, not ours: SceneID, Region and Prefix are the
// parameters its SDK needs, and they are public values (the vendor serves them
// to any browser).  They are never credentials, which is why the captcha page
// is allowed to receive them in a query string.
type CaptchaScene struct {
	// Required means "this action is gated and the panel should run the SDK".
	// A module that has the capability but needs nothing for this particular
	// action answers Required=false, and the panel skips the browser step.
	//
	// How a module decides is its own business -- zcode keys it on having a
	// scene id and no local solver -- but it must not be optimistic: a false
	// here means the action runs with no token at all.
	Required bool `json:"required"`
	// Enabled mirrors the vendor's own captcha switch, when it publishes one.
	// It is reported for a human to read, not obeyed: whether a missing token
	// is tolerated is a property of the vendor's endpoint, not of its config.
	Enabled bool   `json:"enabled"`
	SceneID string `json:"scene_id,omitempty"`
	Region  string `json:"region,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
	// Note is shown to the operator when the scene is unusable or surprising --
	// for instance when the vendor did not publish a scene id.  It is prose for
	// a human, never a machine-readable code.
	Note string `json:"note,omitempty"`
}

// CaptchaProvider is implemented by a client whose actions can require a
// browser-minted token.  action is the CheckinAction ID the operator clicked
// ("" for the module's default), so one module can gate some actions and not
// others.
//
// It must be cheap and side-effect free: the panel calls it on every click, not
// on page load, but a module that answers by fetching the vendor's config
// should cache that answer the same way it caches anything else.
type CaptchaProvider interface {
	Client
	CaptchaScene(ctx context.Context, action string) (CaptchaScene, error)
}

// CaptchaSolution is one token, minted in a browser, for one call.
type CaptchaSolution struct {
	// Param is the opaque string the vendor's SDK returned.  It is a bearer
	// credential for a single request: do not log it, and do not store it.
	Param string
	// Region is the region the SDK ran in, which some vendors want echoed back
	// beside the param.  Empty means "not supplied".
	Region string
}

// captchaSolutionKey is unexported and zero-sized, so no other package can
// collide with it and no value can be smuggled in under the same key.
type captchaSolutionKey struct{}

// WithCaptchaSolution attaches a browser-minted token to ctx.
//
// The value is trimmed, and a blank one is a no-op that returns ctx unchanged.
// Both halves are deliberate.  Trimming matters because the string arrives from
// a browser form field or an executable's stdout, where a stray newline is the
// normal failure rather than an exotic one; and the blank shortcut makes "the
// caller sent an empty field" and "the caller never ran a captcha" the same
// thing all the way down, so no module has to tell those apart.
func WithCaptchaSolution(ctx context.Context, s CaptchaSolution) context.Context {
	s.Param = strings.TrimSpace(s.Param)
	s.Region = strings.TrimSpace(s.Region)
	if s.Param == "" {
		return ctx
	}
	return context.WithValue(ctx, captchaSolutionKey{}, s)
}

// CaptchaSolutionFrom reports the token attached by WithCaptchaSolution, if
// any.
//
// A module asks for it BEFORE falling back to its own means (a configured
// solver executable, say), because a token the operator just watched succeed
// in their own browser is better evidence than anything the server can produce
// on its own.
func CaptchaSolutionFrom(ctx context.Context) (CaptchaSolution, bool) {
	s, ok := ctx.Value(captchaSolutionKey{}).(CaptchaSolution)
	if !ok || s.Param == "" {
		return CaptchaSolution{}, false
	}
	return s, true
}

// AsCaptchaProvider narrows a Client, following the AsXxx convention.
func AsCaptchaProvider(c Client) (CaptchaProvider, bool) {
	cp, ok := c.(CaptchaProvider)
	return cp, ok
}
