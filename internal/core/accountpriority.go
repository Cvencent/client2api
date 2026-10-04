package core

import "sync"

// Account priorities are process-wide policy, like the platform routing table.
// They are keyed by platform and the account id the module reports to the
// panel.  A missing entry means zero, so every module keeps its old behaviour
// until an operator sets one.
var (
	accountPriorityMu sync.RWMutex
	accountPriorities = map[string]map[string]int{}
)

// SetAccountPriorities replaces the priority table.  The registry calls this on
// startup and on every live config reload, so pools see a change on their next
// pick without a restart.
func SetAccountPriorities(next map[string]map[string]int) {
	copied := make(map[string]map[string]int, len(next))
	for platform, entries := range next {
		if len(entries) == 0 {
			continue
		}
		byID := make(map[string]int, len(entries))
		for id, priority := range entries {
			if id == "" {
				continue
			}
			byID[id] = priority
		}
		if len(byID) > 0 {
			copied[platform] = byID
		}
	}

	accountPriorityMu.Lock()
	accountPriorities = copied
	accountPriorityMu.Unlock()
}

// AccountPriority returns the operator's priority for one account.  Lower
// numbers are tried first; 0 is the default.
func AccountPriority(platform, id string) int {
	accountPriorityMu.RLock()
	defer accountPriorityMu.RUnlock()
	return accountPriorities[platform][id]
}

// LowestPriorityTier keeps only the accounts in the best priority tier.  The
// caller's existing ordering and rotation policy apply inside that tier, so
// setting one account to -1 makes it preferred without turning the pool into a
// strict single-account pin.
func LowestPriorityTier[T any](platform string, items []T, id func(T) string) []T {
	if len(items) <= 1 {
		return items
	}
	best := AccountPriority(platform, id(items[0]))
	for _, item := range items[1:] {
		if p := AccountPriority(platform, id(item)); p < best {
			best = p
		}
	}
	out := make([]T, 0, len(items))
	for _, item := range items {
		if AccountPriority(platform, id(item)) == best {
			out = append(out, item)
		}
	}
	return out
}
