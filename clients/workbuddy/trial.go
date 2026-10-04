package workbuddy

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Trial claiming, ported from the reference internal/upstream/trial.go (MIT).
//
// Realm note: /billing/ide/trial only exists on the international realm.  The CN
// realm has no such endpoint, so the reference refuses to even try, and so does
// this port — an account on the wrong realm gets a Go error, not a network call.
const trialPath = "/billing/ide/trial"

// trialAlreadyMarkers are the two fingerprints of "already claimed": the
// envelope refusal formats the code as `code=14051` (see envelopeError /
// doJSON), while an HTTP-4xx body carries the raw JSON, i.e. `"code":14051`.
var trialAlreadyMarkers = []string{"code=14051", `"code":14051`}

// ClaimTrial claims the one-off IDE trial credit.  It returns claimed=false and
// no error when the account has already taken it: that is an idempotent success,
// not a failure.
func (c *Client) ClaimTrial(ctx context.Context, a *Auth) (bool, error) {
	if a == nil || !a.IsGlobal() {
		return false, errors.New("claim trial: only global accounts")
	}
	if _, err := c.billingJSON(ctx, a, http.MethodPost, trialPath, nil); err != nil {
		var ue *Error
		if errors.As(err, &ue) && trialAlreadyErr(ue.Msg) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// trialAlreadyErr reports whether an upstream message is the "already claimed"
// answer.
func trialAlreadyErr(msg string) bool {
	lower := strings.ToLower(msg)
	for _, marker := range trialAlreadyMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
