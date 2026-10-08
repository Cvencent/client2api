package loomy

import (
	"time"

	"client2api/internal/core"
)

// Batches implements core.BatchPlanner. The onboarding registry is static,
// so the scheduler can discover the growth batch without touching the network.
func (c *Client) Batches() []core.Batch {
	codes := make([]string, 0, len(onboardingRegistry))
	for _, ch := range onboardingRegistry {
		codes = append(codes, ch.Key)
	}
	return []core.Batch{{
		Name:        "growth",
		Codes:       codes,
		PendingOnly: true,
		// The onboarding board is a sequence of human-looking first-run
		// chores.  Reporting all eight within a second looks like scripted
		// traffic to the vendor, so leave 8-12s (10s +/-20%) between two
		// completions on the same account.
		TaskGap: 10 * time.Second,
	}}
}
