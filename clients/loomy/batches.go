package loomy

import (
	"time"

	"client2api/internal/core"
)

// The reference timetable's batch names this module declares.  A name that does
// not match a scheduler group can never fire, so they are constants rather than
// string literals scattered through the file.
const (
	batchNameKeepalive = "keepalive"
	batchNameGrowth    = "growth"

	// choreKeepalive is the synthetic task code of the keepalive batch.  It is
	// deliberately not an onboarding key: RunTask dispatches it before the
	// registry is consulted, because the vendor's board never returns it.
	choreKeepalive = "keepalive"
)

// Batches implements core.BatchPlanner.  The onboarding registry is static, so
// the scheduler can discover the growth batch without touching the network.
//
// Two batches are declared:
//
//   - keepalive: one small session-carrying call to the account service per
//     account per slot, the module's only defence against an idle session
//     (Loomy has no refresh endpoint; see keepalive.go).  It rides the shared
//     keepalive timetable (default 22:00 CST, editable in the task centre).
//   - growth: the eight one-off onboarding chores, run pending-only so an
//     already-credited chore is never posted again.
func (c *Client) Batches() []core.Batch {
	codes := make([]string, 0, len(onboardingRegistry))
	for _, ch := range onboardingRegistry {
		codes = append(codes, ch.Key)
	}
	return []core.Batch{
		{
			Name:        batchNameGrowth,
			Codes:       codes,
			PendingOnly: true,
			// The onboarding board is a sequence of human-looking first-run
			// chores.  Reporting all eight within a second looks like scripted
			// traffic to the vendor, so leave 8-12s (10s +/-20%) between two
			// completions on the same account.
			TaskGap: 10 * time.Second,
		},
		{
			Name:  batchNameKeepalive,
			Codes: []string{choreKeepalive},
		},
	}
}
