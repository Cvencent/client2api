package workbuddy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"client2api/internal/core"
)

// panel_tasks.go exposes the chore-board actions the panel serves through the
// optional task interfaces internal/core declares.  The panel reaches them via
// core.CapabilitiesOf, which type-asserts the registered client, so adding these
// methods is the whole wiring -- nothing is registered here and no vendor call
// is invented: every path below reuses the module's own accept/claim/run
// helpers, which already know the miniprogram routing and the anti-abuse
// spacing.
//
// RunTask is deliberately left alone.  It is pinned by the module's own test
// suite and answers a slightly different question (a single run's outcome);
// RunTaskAuto is a second view over the same helpers that reports the read-back
// the panel renders.
var (
	_ core.TaskAccepter     = (*Client)(nil)
	_ core.BulkTaskAccepter = (*Client)(nil)
	_ core.TaskClaimer      = (*Client)(nil)
	_ core.TaskAutoRunner   = (*Client)(nil)
)

const (
	// acceptAllBatch is the reference's per-request cap for a bulk accept.
	acceptAllBatch = 20

	// panelNoAutoAction is the reference's exact 501 sentence.  The panel
	// string-matches it (or rather, maps the error to HTTP 501), so the wording
	// is part of the contract and must not be rephrased.
	panelNoAutoAction = "该任务需要客户端内交互（无对应接口），无法自动完成；请按任务说明在官方客户端操作"

	panelAllAccepted  = "所有任务均已接受"
	panelAcceptFailed = "部分任务接受失败（上游拒绝），可重试"

	panelAlreadyClaimed = "该奖励此前已领取"
	// panelClaimedSkipped is the reference's own wording for the skipped answer.
	panelClaimedSkipped  = "该任务已领取过奖励"
	panelClaimRetryTail  = "；达标但领奖失败，可在任务列表手动点「领取」重试"
	panelClaimedNowTail  = "；已自动领奖 +%d 分 +%d 能"
	panelClaimedOnceTail = "；奖励此前已领取"
)

// panelTaskAuth resolves the account id the panel passes to the credential and
// the module's own parked/enabled rules.  It keeps TaskAccount's wording, which
// already names the id when it is unknown.
func (c *Client) panelTaskAuth(accountID string) (*Auth, error) {
	if c == nil {
		return nil, errors.New("workbuddy: no client")
	}
	c.refreshAccounts(false)
	a, err := c.taskAccount(strings.TrimSpace(accountID))
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, errors.New("workbuddy: no usable account is configured")
	}
	return a, nil
}

// panelSplitCodes groups task codes by channel.  The miniprogram chores live on
// a different endpoint and must go out with the miniprogram platform header;
// folding them into the web accept call is what the reference explicitly does
// not do.  Blank codes are dropped rather than sent upstream.
func panelSplitCodes(codes []string) (web, mp []string) {
	for _, code := range codes {
		trimmed := strings.TrimSpace(code)
		if trimmed == "" {
			continue
		}
		if isMPTaskCode(trimmed) {
			mp = append(mp, trimmed)
			continue
		}
		web = append(web, trimmed)
	}
	return web, mp
}

// AcceptTasks implements core.TaskAccepter.  One accept call per channel, with
// the channel flag that matches the codes in it.
//
// An empty code list is a caller error: the panel already rejects that request
// with 400, so accepting it silently here would only hide a broken caller.
func (c *Client) AcceptTasks(ctx context.Context, accountID string, codes []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	web, mp := panelSplitCodes(codes)
	if len(web) == 0 && len(mp) == 0 {
		return errors.New("workbuddy: no task codes were given")
	}
	a, err := c.panelTaskAuth(accountID)
	if err != nil {
		return err
	}
	if len(web) > 0 {
		if err := c.acceptTasks(ctx, a, web, false); err != nil {
			return err
		}
	}
	if len(mp) > 0 {
		if err := c.acceptTasks(ctx, a, mp, true); err != nil {
			return err
		}
	}
	return nil
}

// panelTaskSettled reports whether a board row still needs an accept call.
// growthTask exposes its accept state as AcceptStatus (there is no Claimed
// bool), so the reference's skip rule is expressed through that field plus the
// module's own claimed()/Locked signals.
func panelTaskSettled(t growthTask) bool {
	if t.Locked || t.claimed() {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(t.AcceptStatus)) {
	case "accepted", "completed":
		return true
	}
	return false
}

// AcceptAllTasks implements core.BulkTaskAccepter.  It reads the module's own
// merged board (web + miniprogram), skips the chores that are already settled,
// and then accepts the rest in the reference's batches, keeping the reference's
// ~1.05s gap between consecutive successful batches (reportGap is the module's
// existing constant for exactly that cadence).
//
// A partial refusal is not an error: the refused codes land in Failed and the
// panel tells the operator to retry.  Only a board that cannot be read at all
// is an error.
func (c *Client) AcceptAllTasks(ctx context.Context, accountID string) (core.BulkAccept, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	a, err := c.panelTaskAuth(accountID)
	if err != nil {
		return core.BulkAccept{}, err
	}

	rows, err := c.mergedTaskRows(ctx, a)
	if err != nil {
		return core.BulkAccept{}, fmt.Errorf("workbuddy: list the task board: %w", err)
	}

	var web, mp []string
	for _, row := range rows {
		if panelTaskSettled(row.task) {
			continue
		}
		code := strings.TrimSpace(row.task.TaskCode)
		if code == "" {
			continue
		}
		if isMPTaskCode(code) {
			mp = append(mp, code)
			continue
		}
		web = append(web, code)
	}

	if len(web) == 0 && len(mp) == 0 {
		return core.BulkAccept{Message: panelAllAccepted}, nil
	}

	res := core.BulkAccept{}
	// The web stage first, then the miniprogram stage: the reference runs them
	// as two stages and only ever reports one combined failure list.
	webAccepted, webFailed := c.panelAcceptBatches(ctx, a, web, false)
	mpAccepted, mpFailed := c.panelAcceptBatches(ctx, a, mp, true)
	res.Accepted = webAccepted + mpAccepted
	res.Failed = append(webFailed, mpFailed...)
	if len(res.Failed) > 0 {
		res.Message = panelAcceptFailed
		return res, nil
	}
	res.Message = fmt.Sprintf("已接受 %d 个任务", res.Accepted)
	return res, nil
}

// panelAcceptBatches accepts codes in batches of acceptAllBatch, sleeping the
// module's anti-abuse cadence after each successful batch that has more work
// behind it.  A refused batch goes to failed and the next batch is still tried,
// because a single bad code must not strand the rest of the board.
//
// The accepted count and the failed list are the caller's to combine: the panel
// reports one summary for both channels.
func (c *Client) panelAcceptBatches(ctx context.Context, a *Auth, codes []string, mp bool) (int64, []string) {
	var accepted int64
	var failed []string
	for i := 0; i < len(codes); i += acceptAllBatch {
		end := i + acceptAllBatch
		if end > len(codes) {
			end = len(codes)
		}
		batch := codes[i:end]
		if err := c.acceptTasks(ctx, a, batch, mp); err != nil {
			c.deps.Log("workbuddy: accepting %d tasks failed: %v", len(batch), err)
			failed = append(failed, batch...)
			continue
		}
		accepted += int64(len(batch))
		if end >= len(codes) {
			continue
		}
		if !sleepCtx(ctx, reportGap) {
			// The caller gave up: the remaining codes were never offered, so
			// they belong in the retry list rather than in accepted.
			failed = append(failed, codes[end:]...)
			break
		}
	}
	return accepted, failed
}

// ClaimTask implements core.TaskClaimer.  It routes the claim to the channel
// that owns the code, and treats both of the vendor's "nothing to collect"
// answers as idempotent success -- the panel answers 200 for those, it is not a
// failure.
func (c *Client) ClaimTask(ctx context.Context, accountID, code string) (core.TaskClaim, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return core.TaskClaim{}, errors.New("workbuddy: no task code was given")
	}
	a, err := c.panelTaskAuth(accountID)
	if err != nil {
		return core.TaskClaim{}, err
	}
	claim, err := c.claimReward(ctx, a, trimmed, isMPTaskCode(trimmed))
	if err != nil {
		return core.TaskClaim{}, err
	}
	if claim.AlreadyClaimed || (claim.Credit == 0 && claim.Energy == 0) {
		return core.TaskClaim{AlreadyClaimed: true, Message: panelAlreadyClaimed}, nil
	}
	return core.TaskClaim{
		Credit:  claim.Credit,
		Energy:  claim.Energy,
		Message: fmt.Sprintf("已领取 %d 积分 %d 能量", claim.Credit, claim.Energy),
	}, nil
}

// RunTaskAuto implements core.TaskAutoRunner.  It is the same accept ->
// make-progress -> claim sequence RunTask drives, but it reports the read-back
// the panel renders instead of a single overall outcome:
//
//   - ProgressBefore/ProgressAfter are the module's own progress text;
//   - Claimable is the board's own claimable flag after the run;
//   - Attempt says whether an action was actually exercised;
//   - Claimed is set only when this call collected the reward;
//   - ClaimError is set when the chore completed but collecting failed, which is
//     what makes the panel offer the claim button again;
//   - Skipped is set when the chore was already rewarded and nothing ran.
//
// A chore with no automated action is an error carrying the reference's exact
// sentence, which the panel maps to HTTP 501.
func (c *Client) RunTaskAuto(ctx context.Context, accountID, code string) (res core.AutoTaskResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	trimmed := strings.TrimSpace(code)
	res = core.AutoTaskResult{TaskResult: core.TaskResult{
		Code:      trimmed,
		AccountID: strings.TrimSpace(accountID),
		At:        time.Now().UTC().Format(time.RFC3339),
	}}
	if c == nil {
		return res, errors.New("workbuddy: no client")
	}
	// The account is resolved first, exactly like the reference: an unknown
	// account is an account problem, not a "this chore is not automatable"
	// problem, and the two answer with different statuses.
	a, aerr := c.panelTaskAuth(accountID)
	if aerr != nil {
		return res, aerr
	}
	res.AccountID = a.ID()

	runner, ok := taskRunnerFor(trimmed)
	if !ok {
		// No runner means the module has no automated action for this chore; the
		// panel turns this exact message into a 501 and tells the operator to do
		// it in the official client.
		return res, errors.New(panelNoAutoAction)
	}

	start := time.Now()
	defer func() { res.ElapsedMS = time.Since(start).Milliseconds() }()

	mp := runner.mp || isMPTaskCode(trimmed)

	// Read before running: an already-rewarded chore is reported as skipped
	// rather than attempted, and a chore this account does not have must not
	// look like a failed run.
	before, rerr := c.readTask(ctx, a, trimmed, mp)
	if rerr != nil {
		c.pool.MarkFailure(a, rerr)
		return res, rerr
	}
	if before == nil {
		// The reference answers this with a 404 rather than a result body, so it
		// stays an error here too.
		msg := fmt.Sprintf("workbuddy: this account's board has no task %q", trimmed)
		res.Error = core.Redact(describeFailure(errors.New(msg)))
		return res, errors.New(msg)
	}
	res.ProgressBefore = panelProgressText(before)
	res.Claimable = before.claimable()
	if before.claimed() {
		// Already rewarded: nothing was attempted, and the panel renders this as
		// "该任务已领取过奖励".
		res.Skipped = true
		res.OK = true
		res.ProgressAfter = res.ProgressBefore
		res.Message = panelClaimedSkipped
		return res, nil
	}

	res.Attempt = true
	note, rerr := runner.run(ctx, c, a, before)
	if rerr != nil {
		var ref *taskRefusal
		if errors.As(rerr, &ref) {
			res.OK = false
			res.Error = core.Redact(ref.msg)
			res.Message = firstNonEmpty(note, ref.msg)
			return res, nil
		}
		c.pool.MarkFailure(a, rerr)
		res.OK = false
		res.Error = core.Redact(describeFailure(rerr))
		return res, nil
	}

	// The run reported events; drop the cached board so the re-read can see the
	// scorer's answer.
	c.invalidateTaskCache(a)
	after := c.waitClaimable(ctx, a, trimmed, mp)
	if after != nil {
		res.ProgressAfter = panelProgressText(after)
	}

	switch {
	case after != nil && after.claimed():
		c.pool.MarkSuccess(a)
		res.OK = true
		res.Message = firstNonEmpty(note, "the chore is complete")
		return res, nil

	case after != nil && after.claimable():
		res.Claimable = true
		claim, cerr := c.claimReward(ctx, a, trimmed, mp)
		res.OK = true
		if cerr != nil {
			// The chore was completed; only the collection failed.  The panel
			// keeps that separate so it can re-offer the claim button.
			res.ClaimError = core.Redact(describeFailure(cerr))
			res.Message = firstNonEmpty(note, "the chore is complete") + panelClaimRetryTail
			return res, nil
		}
		res.Claimed = true
		res.Credit, res.Energy = claim.Credit, claim.Energy
		if claim.AlreadyClaimed || (claim.Credit == 0 && claim.Energy == 0) {
			res.Message = firstNonEmpty(note, "the chore is complete") + panelClaimedOnceTail
			return res, nil
		}
		res.Message = strings.TrimSpace(firstNonEmpty(note, "the chore is complete") +
			fmt.Sprintf(panelClaimedNowTail, claim.Credit, claim.Energy))
		return res, nil

	case after != nil:
		// Events were reported but the target is not credited yet: scoring is
		// asynchronous, so this is progress, not a failure.
		c.pool.MarkSuccess(a)
		res.OK = true
		res.Message = taskProgressTail(note, after)
		return res, nil

	default:
		c.pool.MarkSuccess(a)
		res.OK = true
		res.Message = firstNonEmpty(note, "the events were reported") +
			"; the board could not be re-read to confirm the progress"
		return res, nil
	}
}

// panelProgressText renders a board row the way the panel shows it: the target
// pair when the chore has one, otherwise the accept status (with an already
// claimed row reading as "claimed").  It mirrors the reference's taskProgressText
// so a progress_before/progress_after pair is directly comparable.
func panelProgressText(t *growthTask) string {
	if t == nil {
		return ""
	}
	if t.Target > 0 {
		return fmt.Sprintf("%d/%d", t.Current, t.Target)
	}
	if t.claimed() {
		return "claimed"
	}
	return strings.TrimSpace(t.AcceptStatus)
}
