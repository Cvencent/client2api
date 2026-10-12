package loomy

import (
	"testing"

	"client2api/internal/core"
)

func growthBatch(t *testing.T, batches []core.Batch) core.Batch {
	t.Helper()
	for _, b := range batches {
		if b.Name == batchNameGrowth {
			return b
		}
	}
	t.Fatalf("no growth batch in %+v", batches)
	return core.Batch{}
}

func TestLoomyPlansTheGrowthBatch(t *testing.T) {
	c := &Client{}
	b := growthBatch(t, c.Batches())
	if !b.PendingOnly {
		t.Fatal("growth batch must skip tasks the vendor already reports complete")
	}
	if b.TaskGap <= 0 {
		t.Fatal("growth batch must pace chores within one account")
	}
	if len(b.Codes) != len(onboardingRegistry) {
		t.Fatalf("codes = %d, want %d", len(b.Codes), len(onboardingRegistry))
	}
	want := make(map[string]bool, len(onboardingRegistry))
	for _, ch := range onboardingRegistry {
		want[ch.Key] = true
	}
	for _, code := range b.Codes {
		if !want[code] {
			t.Fatalf("unexpected code %q", code)
		}
	}
}

func TestLoomyPlansTheKeepaliveBatch(t *testing.T) {
	c := &Client{}
	var found bool
	for _, b := range c.Batches() {
		if b.Name != batchNameKeepalive {
			continue
		}
		found = true
		if len(b.Codes) != 1 || b.Codes[0] != choreKeepalive {
			t.Fatalf("keepalive codes = %v, want [%q]", b.Codes, choreKeepalive)
		}
		// The heartbeat is one cheap call per account and has no board row to
		// poll, so it must not be pending-only.
		if b.PendingOnly {
			t.Fatal("keepalive batch must not be pending-only")
		}
	}
	if !found {
		t.Fatalf("Batches() = %+v, want a %q batch", c.Batches(), batchNameKeepalive)
	}
}

func TestLoomyGrowthBatchIsAPlannerAndTaskProvider(t *testing.T) {
	var c *Client
	_ = core.BatchPlanner(c)
	_ = core.TaskProvider(c)
}

func TestLoomyPlannedBatchesIncludeTheDailyCheckin(t *testing.T) {
	c := &Client{}
	batches := core.PlannedBatches(c)
	if len(batches) != 3 {
		t.Fatalf("PlannedBatches = %+v, want growth, keepalive plus checkin", batches)
	}
	names := []string{batches[0].Name, batches[1].Name, batches[2].Name}
	want := []string{batchNameGrowth, batchNameKeepalive, core.CheckinBatchName}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("PlannedBatches order = %v, want %v", names, want)
		}
	}
	if !core.IsCheckinBatch(c, batches[2]) {
		t.Fatal("the scheduled Loomy check-in was not recognised as the synthetic check-in batch")
	}
}
