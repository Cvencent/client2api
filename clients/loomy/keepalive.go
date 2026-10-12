package loomy

import (
	"context"
	"strings"
	"time"

	"client2api/internal/core"
)

// keepalive.go is the module's low-cost heartbeat: it is what stops a Loomy
// session from sitting completely untouched for a whole day.
//
// WHY A HEARTBEAT AT ALL.  A Loomy session is minted by a login and carries no
// refresh token, and the vendor exposes no refresh endpoint -- there is nothing
// to renew *with*.  What the vendor's own desktop client does have is a
// session-carrying call on the account service, getUserInfo:
//
//	POST {accountBase}/userinfo/query/baseInfo   {"base":{...},"param":{"session":"..."}}
//
// signed with the same product AccessKey that signs a login.  That call is the
// cheapest thing the account service answers for a live session, so this chore
// reproduces exactly it on the shared keepalive timetable.  Anything the
// business host sees is NOT enough on its own: the balance sweep already hits
// /points/records every few minutes and sessions still lapsed, so the heartbeat
// deliberately talks to the account service that owns the session instead.
//
// IT IS HONEST ABOUT ITS LIMITS.  A heartbeat can keep an *idle* session warm
// only if the vendor's lifetime slides on use.  If the vendor enforces an
// absolute expiry, or invalidates the session for another reason (a second
// login on the same account, for example), this call changes nothing and the
// session still dies; the heartbeat's real value there is that the next 100002
// or 020002 is seen early and the account is parked for a manual re-login.  It
// never mints or renews a credential.

// heartbeatSession performs the one account-service call the desktop client
// makes with a session (getUserInfo).  A dead session is answered with the
// vendor's own auth code, which postAccount turns into an *apiError.
func (u *upstream) heartbeatSession(ctx context.Context, session string) error {
	_, err := u.postAccount(ctx, "/userinfo/query/baseInfo", map[string]any{"session": session})
	return err
}

// runScheduledKeepalive is the keepalive chore's runner.  RunTask dispatches it
// before the onboarding registry is consulted, because "keepalive" is not a
// board key.
//
// A vendor refusal is a result with OK false; a transport failure is reported
// but deliberately does NOT cool the account down.  The heartbeat is a
// background nicety, and a hiccup on the account host must not take a working
// chat credential out of the pool.  Only a genuine "login invalid" answer is
// acted on, because that is evidence about the session itself.
func (c *Client) runScheduledKeepalive(ctx context.Context, accountID string) (core.TaskResult, error) {
	acc, ok, err := c.taskAccount(accountID)
	if err != nil {
		return core.TaskResult{}, err
	}
	res := core.TaskResult{Code: choreKeepalive, At: time.Now().UTC().Format(time.RFC3339)}
	if !ok {
		res.Error = "loomy: no account to keep alive; import a session first"
		return res, nil
	}
	res.AccountID = acc.ID

	// A credential the vendor already rejected, one the operator parked, or one
	// with no token has nothing to keep alive: a heartbeat would only spend a
	// call to learn what the pool already knows.
	if acc.dead || !acc.Enabled || strings.TrimSpace(acc.AccessToken) == "" {
		res.Skipped = true
		res.Message = "the account is parked or disabled; skipping the keepalive heartbeat"
		return res, nil
	}

	start := time.Now()
	herr := c.up.heartbeatSession(ctx, acc.AccessToken)
	res.ElapsedMS = time.Since(start).Milliseconds()
	if herr != nil {
		res.Error = redactErr(herr)
		if failureKind(herr) == core.FailureSessionDead {
			res.Error = "the session is expired or rejected; log in again and import the new token"
			c.penalise(acc.ID, herr, time.Now().UTC())
			return res, nil
		}
		res.Message = "the keepalive heartbeat was not accepted"
		return res, nil
	}

	// The account service accepted the session, which is the closest thing to a
	// renewal this vendor offers.  Record the use so the pool knows the
	// credential is alive; reset deliberately leaves `dead` alone, so a
	// heartbeat can never revive a parked account.
	c.store.reset(acc.ID, time.Now().UTC())
	res.OK = true
	res.Message = "the account service accepted the session; keepalive heartbeat sent"
	return res, nil
}
