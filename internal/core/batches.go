package core

import (
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Scheduled batches (optional).
//
// The reference gateway ships a background scheduler that runs the official
// client's daily chores on a per-hour-of-day timetable: check in, run the
// travel streak, post the activity, keep the session alive, take the black-cat
// night slot, and collect the growth-centre rewards.  Every one of those
// batches is a vendor-specific set of task codes, and the scheduler must not
// learn any of them.
//
// A module therefore *declares* its own batches through this optional
// capability and the generic scheduler only decides *when* to call RunTask.
// A module that does not implement BatchPlanner is simply never scheduled,
// which keeps the capability additive in exactly the same way as TaskProvider.
// ---------------------------------------------------------------------------

// Batch is one named set of chores that runs as a unit at a scheduled hour.
type Batch struct {
	// Name is a stable identifier.  The reference names are "checkin",
	// "travel", "activity", "keepalive", "blackcat" and "growth"; a module is
	// free to use others, and an operator enables them by name.
	Name string `json:"name"`
	// Codes are the task codes (as returned by Tasks) that make up the batch,
	// in the order they should run.  A code the module refuses at runtime --
	// already claimed, prerequisite missing -- is a TaskResult with OK false
	// and is not an error: the next code still runs.
	Codes []string `json:"codes"`
	// AccountGap is the minimum wait between two consecutive accounts.
	//
	// Pacing is protocol, not politeness.  The vendor's anti-cheat rolls back
	// sub-second bursts wholesale, so a batch that fires every account at once
	// loses the rewards it was meant to earn.  Zero means the scheduler's
	// default.
	AccountGap time.Duration `json:"account_gap,omitempty"`
	// TaskGap is the minimum wait between two consecutive codes within one
	// account.  Zero keeps the historical behaviour: a batch's codes run
	// immediately, one after another.  Modules whose chores look like a human
	// sequence (Loomy's first-run onboarding, for example) set this so the
	// vendor does not see a single-second burst.
	TaskGap time.Duration `json:"task_gap,omitempty"`
	// Settle is how long to wait after a run before trusting a read-back.
	// Upstream scoring lags by several seconds, so a batch that immediately
	// re-reads the board sees stale progress and would double-run.
	Settle time.Duration `json:"settle,omitempty"`
	// Claim names codes whose reward must be claimed after the batch's runs.
	// They are appended to Codes when the batch executes, in this order.
	Claim []string `json:"claim,omitempty"`
	// PendingOnly means the executor must re-read Tasks for each account and
	// run only codes that are still actionable. It is for one-off growth
	// chores: running an already-claimed chore is harmless but pollutes the
	// run journal and looks like work that still needs doing.
	PendingOnly bool `json:"pending_only,omitempty"`
	// Gate names a task code that must be present and *incomplete* before the
	// batch is worth running at all (the reference gates the travel batch on
	// the buddy agreement/first-chat chores).  Empty means "always run".
	Gate string `json:"gate,omitempty"`
}

// BatchPlanner is an optional capability: a module that can describe its own
// scheduled batches.
//
// Batches must be cheap and side-effect free: the scheduler calls it on every
// tick, and the panel may call it on every page load.  Returning an empty
// slice is legitimate and means "nothing to schedule".
type BatchPlanner interface {
	Batches() []Batch
}

// AsBatchPlanner narrows a registered client.
func AsBatchPlanner(c Client) (BatchPlanner, bool) {
	bp, ok := c.(BatchPlanner)
	return bp, ok
}

// CheckinBatchName is the batch name PlannedBatches gives a module that can
// check in for one account but declares no batches of its own.
//
// It is chosen deliberately to match the reference's own "checkin" batch, so
// the synthetic one inherits the shared check-in timetable and any per-platform
// override the operator already wrote for that name.
const CheckinBatchName = "checkin"

// PlannedBatches reports every batch a module should appear under in the task
// centre and the scheduler.
//
// A module that declares its own batches keeps them, but a CheckinProvider
// still gets the synthetic check-in when it has no declared batch under that
// name.  This keeps the panel's per-account check-in button and the daily
// scheduled check-in as the same chore even for modules such as Loomy that
// also schedule unrelated growth chores.
func PlannedBatches(c Client) []Batch {
	var out []Batch
	declaredCheckin := false
	if bp, ok := AsBatchPlanner(c); ok {
		if bs := bp.Batches(); len(bs) > 0 {
			out = append(out, bs...)
			for _, b := range bs {
				if strings.EqualFold(strings.TrimSpace(b.Name), CheckinBatchName) {
					declaredCheckin = true
					break
				}
			}
		}
	}
	if !declaredCheckin {
		if _, ok := AsCheckinProvider(c); ok {
			out = append(out, Batch{Name: CheckinBatchName, Codes: []string{CheckinBatchName}})
		}
	}
	return out
}

// IsCheckinBatch reports whether b is the synthetic check-in batch, i.e. the
// one PlannedBatches appended because the module does not declare a batch
// named "checkin" itself.
//
// The executor must ask this instead of testing b.Name: a module that declares
// its own "checkin" batch means task codes to run (zcode, workbuddy and
// minimaxcode all do), while the synthetic one means CheckinProvider calls.
func IsCheckinBatch(c Client, b Batch) bool {
	if b.Name != CheckinBatchName || len(b.Codes) != 1 || b.Codes[0] != CheckinBatchName {
		return false
	}
	if bp, ok := AsBatchPlanner(c); ok {
		for _, declared := range bp.Batches() {
			if strings.EqualFold(strings.TrimSpace(declared.Name), CheckinBatchName) {
				return false
			}
		}
	}
	_, ok := AsCheckinProvider(c)
	return ok
}

// BatchOf finds one batch by name, reporting false when the client does not
// plan batches at all or does not define that name.
func BatchOf(c Client, name string) (Batch, bool) {
	for _, b := range PlannedBatches(c) {
		if b.Name == name {
			return b, true
		}
	}
	return Batch{}, false
}
