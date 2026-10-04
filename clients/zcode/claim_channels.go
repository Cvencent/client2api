package zcode

import (
	"fmt"
	"strings"

	"client2api/internal/core"
)

// claimChannelFor resolves which credential actually serves the plan-billing
// endpoints for an account the operator selected.
//
// The panel groups credentials by Identity, so the row the operator clicked may
// be either the plan JWT or the coding-plan API key.  The vendor's billing
// endpoints want the JWT; the monitor endpoint wants the API key.  Both are
// channels of the same account, so either row must be able to start a claim.
func (c *Client) claimChannelFor(acct *Account) (*Account, error) {
	if acct == nil {
		return nil, fmt.Errorf("zcode: account is required")
	}
	if acct.Mode == modeJWT && strings.TrimSpace(acct.jwt) != "" {
		return acct, nil
	}
	for _, sibling := range c.siblingAccountsFor(acct) {
		if sibling.Mode == modeJWT && strings.TrimSpace(sibling.jwt) != "" {
			return sibling, nil
		}
	}
	return nil, fmt.Errorf("%w: plan billing needs a ZCode plan (jwt) credential; %s has no jwt channel",
		core.ErrNotConfigured, acct.ID)
}
