package workbuddy

import (
	"strings"

	"client2api/internal/core"
)

// availability.go implements core.ModelAvailabilityProvider for the router's
// pre-flight check.
//
// The account census cannot answer this question.  The vendor parks one model
// on a credential (11102 "model not available", 6004 "model rate limited")
// while leaving that credential perfectly usable for every other model, so a
// pool can report every account ready and still have nothing to hand out for
// the id a caller asked for.  Reporting that per request is what lets the
// router skip this platform for a sibling before it spends a call that could
// only repeat the pick.
var _ core.ModelAvailabilityProvider = (*Client)(nil)

// Available reports whether any account could take model right now.  A nil
// error means "try it"; otherwise the returned refusal is the same one Chat
// would have produced for that state.
//
// It reads the pool only: no network call, no account slot and no rotation
// state are touched.  refreshAccounts is the mtime-gated reload Chat already
// performs first, so the two paths always see the same credentials.
func (c *Client) Available(model string, free bool) error {
	if c == nil || c.pool == nil {
		return nil
	}
	route := routeModel(strings.TrimSpace(model))
	if route.bare == "" {
		return core.ErrUnsupported
	}
	c.refreshAccounts(false)
	if c.pool.Len() == 0 {
		return core.ErrNotConfigured
	}
	for _, realm := range route.realms {
		if c.pool.CanServe(route.bare, realm, free) {
			return nil
		}
	}
	// Mirror the tail of Chat, so the router and the caller see the same
	// classification whether the verdict came from this probe or from the call
	// itself: an all-full pool is backpressure, a realm that holds accounts but
	// cannot serve this model is platform exhaustion.
	if c.pool.AllFull() {
		return core.ErrBusy
	}
	for _, realm := range route.realms {
		if c.pool.HasRealm(realm) {
			return core.ErrPlatformExhausted
		}
	}
	return core.ErrNotConfigured
}
