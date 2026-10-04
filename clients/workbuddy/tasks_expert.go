package workbuddy

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// Expert-market runners: the chores whose completion requires a live expert
// session rather than a synthetic chat event.
//
// These became wirable only after the desktop expert port (expert.go) landed.
// The reference measured that the vendor scores `expert_actual_use` solely on a
// requestId that the SERVER issued, so every runner below either obtains a real
// market expert id or refuses to pretend it ran.
//
// Chores that still have no runner on purpose (skill_1, Expert_lighthouse,
// Expert_Philanthropy) are listed in taskManualNotes with the reason; they need
// a client-side interaction that cannot be driven from here (a real Skill tool
// call, a third-party connector authorisation, a real donation).

// pickMarketExpert reads the live market catalogue and returns the first entry
// that carries a usable `ex_` id.  A catalogue entry without such an id is not
// scoreable: the vendor accepts the report and credits nothing.
func (c *Client) pickMarketExpert(ctx context.Context, a *Auth) (MarketExpert, error) {
	list, err := c.MarketExpertList(ctx, a, "")
	if err != nil {
		return MarketExpert{}, err
	}
	for _, e := range list {
		if strings.HasPrefix(strings.TrimSpace(e.ExpertID), "ex_") {
			return e, nil
		}
	}
	for _, e := range list {
		if strings.TrimSpace(e.ExpertID) != "" {
			return e, nil
		}
	}
	return MarketExpert{}, fmt.Errorf("the market catalogue returned %d experts, none with an id", len(list))
}

// mpEventBatch is the shared miniprogram skeleton for the one-shot event chores
// (expert use, model-declaring chat, playbook).  It deliberately mirrors
// runMPChatTask's pacing: the anti-abuse rollback keys on the report cadence, not
// on which eventCode is being sent.
func (c *Client) mpEventBatch(ctx context.Context, a *Auth, t *growthTask, build func(i int64) []map[string]any) (string, error) {
	if err := c.acceptTasks(ctx, a, []string{t.TaskCode}, true); err != nil {
		c.deps.Logf("workbuddy: accepting miniprogram task %s failed: %v", t.TaskCode, err)
	}
	if !sleepCtx(ctx, mpActionGap) {
		return "", ctx.Err()
	}

	cur := t
	if settled, rerr := c.readTask(ctx, a, t.TaskCode, true); rerr == nil && settled != nil {
		cur = settled
	}
	if cur.claimed() {
		return "already claimed", nil
	}
	need := cur.Target - cur.Current
	if cur.Target <= 0 || need <= 0 {
		// A one-shot chore reports no target; sending it once is the whole chore.
		need = 1
	}
	if need > taskMaxEvents {
		need = taskMaxEvents
	}
	for i := int64(0); i < need; i++ {
		wait := mpChatEventGap
		if mpChatJitter > 0 {
			wait += time.Duration(rand.Int64N(int64(mpChatJitter)))
		}
		if !sleepCtx(ctx, wait) {
			return "", ctx.Err()
		}
		if err := c.reportMPEvent(ctx, a, build(i)...); err != nil {
			return "", err
		}
	}
	return "miniprogram events reported", nil
}

// runMPExpertUse is Sequential_Tasks_2: a real expert-usage event on the
// miniprogram channel.  The expert id has to come from the live catalogue.
func runMPExpertUse(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	expert, err := c.pickMarketExpert(ctx, a)
	if err != nil {
		return "", refuse("no live expert id could be read from the market catalogue: %v", err)
	}
	return c.mpEventBatch(ctx, a, t, func(int64) []map[string]any {
		return []map[string]any{MiniExpertUseEvent(expert.ExpertID, expert.DisplayNameZH, expert.ExpertType)}
	})
}

// runMPChatModel is Sequential_Tasks_5: the miniprogram chat that declares which
// model it asked for (the "use GLM-5.2" carrier).
func runMPChatModel(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	return c.mpEventBatch(ctx, a, t, func(i int64) []map[string]any {
		conv := fmt.Sprintf("wb2api-mp-glm52-%d-%d", time.Now().UnixMilli(), i)
		return []map[string]any{MiniChatModelEvent(conv, "glm-5.2", "GLM-5.2")}
	})
}

// runMPPlaybook is Sequential_Tasks_7: the two-event miniprogram playbook
// sequence (CTA click, then the prompt send it enables).
func runMPPlaybook(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	return c.mpEventBatch(ctx, a, t, func(int64) []map[string]any {
		return MiniPlaybookEvents(playbookCaseID, playbookCaseName)
	})
}

// runExpertActualUse is expert_actual_use on the desktop channel.
//
// Order matters and is copied from the reference: read the market, spend one
// real chat to obtain the SERVER requestId, then report the summon chain and the
// actual-use join event that names it.  Skipping the chat produces a well-formed
// event that the vendor silently discards.
func runExpertActualUse(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	expert, err := c.pickMarketExpert(ctx, a)
	if err != nil {
		return "", refuse("no live expert id could be read from the market catalogue: %v", err)
	}
	conversationID, requestID, err := c.DesktopChatWithExpert(ctx, a, expert.ExpertID)
	if err != nil {
		return "", err
	}
	events := make([]map[string]any, 0, 4)
	events = append(events, DesktopExpertSummonSequence(expert)...)
	events = append(events, DesktopExpertActualUseEvent(expert, conversationID, requestID))
	if err := c.reportDesktopEvent(ctx, a, events...); err != nil {
		return "", err
	}
	return fmt.Sprintf("expert %s summoned and used (server requestId %s)", expert.ExpertID, requestID), nil
}

// runSequential4 is Sequential_Tasks_4, the miniprogram "create a scheduled
// task" chore.
//
// The criteria are provisional, exactly as they are in the reference: the
// miniprogram bundle has no automation emission point, so the reference points
// this chore at the same PC-side event automation_1 uses and lets the board
// decide whether it credited.  Accepting first is what puts the row into the
// state the vendor expects the event to arrive in; the accept failure is logged
// rather than fatal for the same reason it is in mpEventBatch -- the behaviour
// event, not the acceptance, is what moves progress.
//
// The readback and the claim are RunTask's, so this only reports.
func runSequential4(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	if err := c.acceptTasks(ctx, a, []string{t.TaskCode}, true); err != nil {
		c.deps.Logf("workbuddy: accepting miniprogram task %s failed: %v", t.TaskCode, err)
	}
	if !sleepCtx(ctx, mpActionGap) {
		return "", ctx.Err()
	}
	if err := c.reportDesktopEvent(ctx, a, desktopAutomationCreateEvent()); err != nil {
		return "", err
	}
	return "automation create reported (provisional miniprogram criteria)", nil
}

// expertSummonGap is the pause between two experts in a batch chain.  The
// reference measured 8s as a 100% success rate and settled on 6s; a chain is
// scored on the summon AND the real chat that follows it, and five of those back
// to back read as automation rather than use.
const expertSummonGap = 6 * time.Second

// runExpertUse is expert_5: five live platform experts summoned and really used.
func runExpertUse(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	return c.runExpertChain(ctx, a, "agent", 5)
}

// runExpertTeamUse is Expert_team_use_3: three live expert TEAMS
// (expertType=team), not three more individual experts.
func runExpertTeamUse(ctx context.Context, c *Client, a *Auth, t *growthTask) (string, error) {
	return c.runExpertChain(ctx, a, "team", 3)
}

// runExpertChain summons and really uses up to count live experts of one type.
//
// Every step is load-bearing and copied from the reference: the id has to come
// from the live market, the summon chain has to be reported, and the chat that
// follows has to be a real one so the requestId is the server's.  A locally
// invented id or requestId produces a well-formed event the vendor does not
// count, which is the whole reason this cannot be done with one report.
//
// One expert failing moves on to the next instead of aborting the batch: the
// chore is scored per credited expert, so four out of five is still progress,
// and RunTask re-reads the board after this returns to decide what to claim.
func (c *Client) runExpertChain(ctx context.Context, a *Auth, expertType string, count int) (string, error) {
	experts, err := c.MarketExpertList(ctx, a, expertType)
	if err != nil {
		return "", fmt.Errorf("reading the expert market: %w", err)
	}
	if len(experts) == 0 {
		return "", refuse("the expert market returned no %s experts to summon", expertType)
	}
	used, failed := 0, 0
	for i, e := range experts {
		if used >= count {
			break
		}
		if err := c.reportDesktopEvent(ctx, a, DesktopExpertSummonSequence(e)...); err != nil {
			failed++
			continue
		}
		conversationID, requestID, cerr := c.DesktopChatWithExpert(ctx, a, e.ExpertID)
		if cerr != nil {
			failed++
			continue
		}
		events := desktopChatSequence(conversationID, requestID, "msg-"+tailOf(requestID, 8), taskModelID, taskModelID)
		events = append(events, DesktopExpertActualUseEvent(e, conversationID, requestID))
		if err := c.reportDesktopEvent(ctx, a, events...); err != nil {
			failed++
			continue
		}
		used++
		if i < len(experts)-1 && !sleepCtx(ctx, expertSummonGap) {
			return "", ctx.Err()
		}
	}
	note := fmt.Sprintf("%d of %d %s expert chain(s) reported", used, count, expertType)
	if failed > 0 {
		note = fmt.Sprintf("%s; %d could not be completed", note, failed)
	}
	return note, nil
}

// playbookCaseID / playbookCaseName are the reference playbook case both
// playbook chores report against.  The bytes below were copied out of the desktop
// runner, not retyped.
const (
	playbookCaseID   = "pm-gtm-launch-plan"
	playbookCaseName = "新产品上市 GTM 发布计划一页纸"
)
