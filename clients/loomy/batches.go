package loomy

import "client2api/internal/core"

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
	}}
}
