package minimaxcode

// MiniMax Code gates the daily sign-in board behind an account-onboarding
// write.  A credential minted by the panel can authenticate every API call and
// still show a zero-credit sign-in panel until this step has run; the desktop
// client performs it once for the "new-install" cohort, immediately after the
// user finishes the first-run guide.

import (
	"context"
	"net/http"
	"strings"
)

const (
	updateOnboardingPath = "/minimax-cloud/api/v1/user/update_onboarding"

	// vibeCoding is the category the desktop client writes on completion.  The
	// route treats the write as idempotent, so retrying a completed account is
	// safe, but a wrong category would make the account look misconfigured in
	// the client's own onboarding state.
	onboardingCategory = "vibe_coding"
)

// completeAccountOnboarding makes a signed-in credential eligible for the
// account features the desktop client unlocks after its first-run guide.
//
// The endpoint only needs the bearer token and an empty account id; it returns
// the same onboarding object the client stores locally.  It is deliberately
// best-effort: a credential that can chat must still be usable when this
// auxiliary write fails, and a later sign-in or check-in retries it.
func (c *Client) completeAccountOnboarding(ctx context.Context, acct *Account) error {
	if c == nil || acct == nil {
		return nil
	}
	if !c.markOnboardingAttempted(acct.ID) {
		return nil
	}

	payload := []byte(`{"completed":true,"category_ids":["` + onboardingCategory + `"]}`)
	if _, _, err := c.cloudCall(ctx, acct, http.MethodPost, updateOnboardingPath, nil, payload); err != nil {
		// Do not cache a failure: the account should be healed on the next
		// attempt rather than staying broken for the life of the process.
		c.forgetOnboarding(acct.ID)
		return err
	}
	return nil
}

// markOnboardingAttempted returns true the first time an account reaches this
// process's onboarding step.
func (c *Client) markOnboardingAttempted(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	c.onboardingMu.Lock()
	defer c.onboardingMu.Unlock()
	if c.onboardingDone == nil {
		c.onboardingDone = map[string]bool{}
	}
	if c.onboardingDone[id] {
		return false
	}
	c.onboardingDone[id] = true
	return true
}

func (c *Client) forgetOnboarding(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	c.onboardingMu.Lock()
	delete(c.onboardingDone, id)
	c.onboardingMu.Unlock()
}
