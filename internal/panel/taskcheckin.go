package panel

import (
	"context"
	"strings"

	"client2api/internal/core"
)

// runTaskAndRecordCheckin is the panel's single RunTask entry point. A declared
// check-in batch runs through TaskProvider, but the account row reads
// CheckinStore, so the result has to be mirrored into that store.
func (p *panel) runTaskAndRecordCheckin(ctx context.Context, tp core.TaskProvider, account, code string) (core.TaskResult, error) {
	var actions []core.CheckinAction
	if cp, ok := core.AsCheckinProvider(tp); ok {
		actions = cp.CheckinActions(ctx)
	}

	res, err := tp.RunTask(ctx, account, code)
	if err != nil {
		return res, err
	}
	p.recordTaskCheckinResult(tp.Name(), account, code, actions, res)
	return res, nil
}

func (p *panel) recordTaskCheckinResult(platform, account, code string, actions []core.CheckinAction, res core.TaskResult) {
	if p == nil || p.checkins == nil {
		return
	}

	action := matchingCheckinAction(actions, res.CheckinAction)
	if action == "" {
		action = matchingCheckinAction(actions, code)
	}
	if action == "" {
		action = matchingCheckinAction(actions, res.Code)
	}
	if action == "" {
		return
	}
	if accountID := strings.TrimSpace(res.AccountID); accountID != "" {
		account = accountID
	}
	p.recordCheckinResult(platform, account, action, core.CheckinResult{
		OK:        res.OK,
		AccountID: account,
		Action:    action,
		Message:   res.Message,
		Error:     res.Error,
		Skipped:   res.Skipped,
		ElapsedMS: res.ElapsedMS,
		At:        res.At,
	})
}

func matchingCheckinAction(actions []core.CheckinAction, code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return ""
	}
	for _, action := range actions {
		if id := strings.TrimSpace(action.ID); id == code {
			return id
		}
	}
	return ""
}
