package workbuddy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Scheduled batches (core.BatchPlanner).
//
// The reference gateway's scheduler runs six daily chores per account.  Only one
// of them is a row on the growth board: check-in, the cat travel, the activity
// report, the token keepalive and the growth scan are endpoints the official
// client calls on its own timetable, and none of them can be discovered by
// reading the board.
//
// core.Batch carries *codes* that the generic scheduler feeds back through
// RunTask, so this module publishes those chores as codes of its own (they start
// no HTTP request by themselves) and teaches RunTask to run them without a board
// row -- see the dispatch in RunTask.  RunTask would otherwise refuse every one
// of them with "this account's board has no task", which is exactly how a
// scheduler can look wired up and still never do anything.
//
// The names are the reference's, because an operator enables a batch by name in
// the schedule config and core.Batch documents those six names as the contract.
// ---------------------------------------------------------------------------

// The reference's batch names.  They are the keys of scheduler.Config's groups,
// so a name that does not match here is a batch that can never fire.
const (
	batchNameCheckin   = "checkin"
	batchNameTravel    = "travel"
	batchNameActivity  = "activity"
	batchNameKeepalive = "keepalive"
	batchNameBlackcat  = "blackcat"
	batchNameGrowth    = "growth"
)

// Scheduled chore codes: chores the vendor's own client does on a schedule and
// that have no growth-board row.  They are deliberately not task codes -- the
// board never returns them -- so they are dispatched before the board read.
const (
	choreCheckin   = "checkin"
	choreTravel    = "travel"
	choreActivity  = "activity"
	choreKeepalive = "keepalive"
	choreGrowth    = "growth"
)

// choreRunner performs one scheduled chore for one account.  There is no board
// row to hand in and no reward to claim: the chore either did its work or it did
// not, and what landed goes on res.Credit/res.Energy.
type choreRunner func(ctx context.Context, c *Client, a *Auth, res core.TaskResult) core.TaskResult

// batchChores is the dispatch table RunTask consults before it reads the board.
var batchChores = map[string]choreRunner{
	choreCheckin:   runScheduledCheckin,
	choreTravel:    runScheduledTravel,
	choreActivity:  runScheduledActivity,
	choreKeepalive: runScheduledKeepalive,
	choreGrowth:    runScheduledGrowth,
}

// batchChoreFor resolves a scheduled chore code.
func batchChoreFor(code string) (choreRunner, bool) {
	fn, ok := batchChores[strings.TrimSpace(code)]
	return fn, ok
}

var _ core.BatchPlanner = (*Client)(nil)

// Batches implements core.BatchPlanner.
//
// It is static on purpose: the scheduler calls it on every tick and the panel
// may call it when it renders the schedule, so it must not touch the network or
// the account pool.  A batch with no usable account simply runs over zero
// accounts, which the scheduler reports as such.
func (c *Client) Batches() []core.Batch {
	return []core.Batch{
		{Name: batchNameCheckin, Codes: []string{choreCheckin}},
		// Adoption runs first: the travel endpoints refuse a trip until the
		// buddy chore has been credited, and first_buddy is the module's board
		// code for it.  Its refusal (already adopted, or no such row) is a
		// result, not an abort, so the travel code still gets its turn.
		{Name: batchNameTravel, Codes: []string{"first_buddy", choreTravel}},
		{Name: batchNameActivity, Codes: []string{choreActivity}},
		{Name: batchNameKeepalive, Codes: []string{choreKeepalive}},
		// The night slot is an ordinary board chore: its runner already
		// enforces the vendor's 23:00-08:00 counting window, so a batch that
		// fires outside it refuses instead of sending chats that do not count.
		{Name: batchNameBlackcat, Codes: []string{"black_cat"}},
		// One code, not a frozen list.  The vendor unlocks the Sequential
		// family one ring per day, so a static list would fire events for
		// chores that are still locked -- precisely the traffic that gets an
		// account rolled back.  The scan happens when the batch runs, because
		// Batches itself must stay cheap.
		{Name: batchNameGrowth, Codes: []string{choreGrowth}},
	}
}

// runScheduledCheckin posts the daily reward.
//
// The international realm has no check-in at all, and the reference scheduler
// deliberately spends no upstream call on a global account ("D4 gate": a realm
// that cannot credit the reward would only produce risk-control noise).  The
// panel's manual check-in still offers the console activity for those accounts;
// the scheduled one refuses and says so, because that is what the reference's
// scheduler does.
func runScheduledCheckin(ctx context.Context, c *Client, a *Auth, res core.TaskResult) core.TaskResult {
	if a.IsGlobal() {
		res.OK = false
		res.Skipped = true
		res.Message = "the international realm credits no check-in; the scheduled check-in skips global accounts"
		return res
	}
	cr := c.checkinCN(ctx, a, core.CheckinResult{
		AccountID: a.ID(),
		Action:    checkinActionCN,
		At:        time.Now().UTC().Format(time.RFC3339),
	})
	res.OK = cr.OK
	res.Error = cr.Error
	res.Message = core.Redact(strings.TrimSpace(cr.Message))
	// The consecutive-login pass runs whether or not today's reward was
	// credited, because the tiers it redeems were earned by the days before
	// today.  It is gated on the pool instead: the reference runs its pass over
	// the accounts still standing, so an account this check-in just parked (a
	// dead session, a WAF block) is left alone rather than spending another
	// handful of calls that can only fail the same way.
	if c.pool.UsableForModel(a.ID(), "") {
		res = c.streakBonus(ctx, a, res)
	}
	if cr.OK {
		res.Message = firstNonEmpty(strings.TrimSpace(cr.Message), "the daily check-in was credited")
		return res
	}
	if res.Message == "" {
		res.Message = "the check-in was refused"
	}
	if res.Error == "" {
		res.Error = "the upstream would not credit the check-in"
	}
	return res
}

// runScheduledTravel walks the cat-travel state machine: an arrived trip is
// claimed, an idle account sends the cat out, a travelling cat is left alone.
//
// TravelState.DailyLimitReached is the vendor's own "one trip per calendar day"
// flag, so honouring it here is what keeps the batch from spending a refusal
// every time it fires.
func runScheduledTravel(ctx context.Context, c *Client, a *Auth, res core.TaskResult) core.TaskResult {
	if a.IsGlobal() {
		res.OK = false
		res.Skipped = true
		res.Message = "the international realm has no cat travel; skipped"
		return res
	}
	st, err := c.TravelStatus(ctx, a)
	if err != nil {
		if buddyTravelLocked(err) {
			res.OK = false
			res.Message = "the travel is locked until the buddy chore has been credited"
			res.Error = core.Redact(describeFailure(err))
			return res
		}
		c.pool.MarkFailure(a, err)
		res.OK = false
		res.Message = "the travel status could not be read"
		res.Error = core.Redact(describeFailure(err))
		return res
	}
	if st == nil {
		res.OK = false
		res.Message = "the vendor answered no travel status"
		return res
	}

	switch strings.ToLower(strings.TrimSpace(st.State)) {
	case "arrived":
		credit, cerr := c.TravelClaim(ctx, a, st.RecordID)
		if cerr != nil {
			res.OK = false
			res.Message = "the arrived trip could not be claimed"
			res.Error = core.Redact(describeFailure(cerr))
			return res
		}
		c.pool.MarkNonChatSuccess(a)
		res.OK = true
		res.Credit = credit
		res.Message = fmt.Sprintf("the arrived trip was claimed (%d credit)", credit)
		return res

	case "traveling":
		c.pool.MarkNonChatSuccess(a)
		res.OK = true
		res.Message = "the cat is still travelling; nothing to do"
		return res
	}

	// idle
	if st.DailyLimitReached {
		res.OK = false
		res.Skipped = true
		res.Message = "today's trip has already been sent"
		return res
	}
	// location_id 1..4 were measured to be equivalent; the reference sends 1.
	if derr := c.TravelDepart(ctx, a, 1); derr != nil {
		if buddyTravelLocked(derr) {
			res.OK = false
			res.Message = "the travel is locked until the buddy chore has been credited"
			res.Error = core.Redact(describeFailure(derr))
			return res
		}
		c.pool.MarkFailure(a, derr)
		res.OK = false
		res.Message = "the cat could not be sent out"
		res.Error = core.Redact(describeFailure(derr))
		return res
	}
	c.pool.MarkNonChatSuccess(a)
	res.OK = true
	res.Message = "the cat was sent out for today's trip"
	return res
}

// runScheduledActivity posts one activity event and then reads the streak back.
//
// The read-back is the point: the vendor answers 200 for a report it drops
// silently (the reference measured this), so a scheduled report that never looks
// again can lose a whole day of streak without anyone noticing.
func runScheduledActivity(ctx context.Context, c *Client, a *Auth, res core.TaskResult) core.TaskResult {
	if a.IsGlobal() {
		res.OK = false
		res.Skipped = true
		res.Message = "the international realm has no activity scoring; skipped"
		return res
	}
	conv := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
	if err := c.reportChatActivity(ctx, a, conv, "", "", ""); err != nil {
		c.pool.MarkFailure(a, err)
		res.OK = false
		res.Message = "the activity report was refused"
		res.Error = core.Redact(describeFailure(err))
		return res
	}
	c.pool.MarkNonChatSuccess(a)
	res.OK = true

	days, derr := c.GrowthStreak(ctx, a)
	switch {
	case derr != nil:
		res.Message = "the activity was reported; the streak could not be read back"
	case days == 0:
		res.Message = "the activity was reported but the streak is still 0 — the vendor may have dropped the report"
	default:
		res.Message = fmt.Sprintf("the activity was reported; the streak is %d day(s)", days)
	}
	return res
}

// runScheduledKeepalive refreshes the access token so the account is not parked
// for inactivity.
//
// Failure goes through the pool's own accounting rather than a local counter:
// one 12153 session-dead answer must not park an account, and the pool is where
// the consecutive-strike rule lives.
func runScheduledKeepalive(ctx context.Context, c *Client, a *Auth, res core.TaskResult) core.TaskResult {
	if strings.TrimSpace(a.RefreshTokenValue()) == "" {
		res.OK = false
		res.Message = "the account has no refresh token, so the session cannot be kept alive"
		return res
	}
	if err := c.up.RefreshToken(a); err != nil {
		c.pool.MarkFailure(a, err)
		res.OK = false
		res.Message = "the token refresh was refused"
		res.Error = core.Redact(describeFailure(err))
		return res
	}
	c.pool.MarkNonChatSuccess(a)
	// RefreshToken updates the account in memory and (by design) leaves
	// persistence to its caller, so that a refresh which never reaches disk is
	// visible in the log rather than silently forgotten.
	if serr := a.SaveAtomic(); serr != nil {
		c.deps.Logf("workbuddy: keepalive refreshed the token but could not save it: %v", serr)
	}
	res.OK = true
	res.Message = "the access token was refreshed"
	return res
}

// runScheduledGrowth scans the board for this account and does whatever is
// actually outstanding: a finished chore is claimed, an automatable one is run
// and claimed, everything else is left for a human.
//
// The board is read at run time, so the batch never fires at a chore the vendor
// has not unlocked yet.  A per-chore failure is counted, never fatal: the scan
// is there to collect what it can.
func runScheduledGrowth(ctx context.Context, c *Client, a *Auth, res core.TaskResult) core.TaskResult {
	board, err := c.Tasks(ctx, a.ID())
	if err != nil {
		res.OK = false
		res.Message = "the growth board could not be read"
		res.Error = core.Redact(describeFailure(err))
		return res
	}

	var ran, claimed, refused int
	for _, t := range board {
		if ctx.Err() != nil {
			break
		}
		switch {
		case t.Claimed:
			continue
		case t.Claimable:
			// Finished but uncollected.  Claiming is the whole job, and it
			// works for a chore with no runner too.
			claim, cerr := c.ClaimTask(ctx, a.ID(), t.Code)
			if cerr != nil {
				refused++
				continue
			}
			if !claim.AlreadyClaimed {
				claimed++
				res.Credit += claim.Credit
				res.Energy += claim.Energy
			}
		case t.Auto:
			out, oerr := c.RunTaskAuto(ctx, a.ID(), t.Code)
			if oerr != nil {
				refused++
				continue
			}
			if !out.Skipped {
				ran++
			}
			if out.Claimed {
				claimed++
			}
			res.Credit += out.Credit
			res.Energy += out.Energy
			// Same cadence the vendor's own script uses between chores.
			if !sleepCtx(ctx, reportGap) {
				break
			}
		}
	}

	res.OK = true
	res.Message = fmt.Sprintf("growth scan: ran %d chore(s), claimed %d, %d refused", ran, claimed, refused)
	return res
}
