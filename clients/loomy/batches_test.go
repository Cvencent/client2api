package loomy

import (
	"testing"

	"client2api/internal/core"
)

func TestLoomyPlansTheGrowthBatch(t *testing.T) {
	c := &Client{}
	batches := c.Batches()
	if len(batches) != 1 {
		t.Fatalf("Batches() = %d batches, want one growth batch", len(batches))
	}
	b := batches[0]
	if b.Name != "growth" {
		t.Fatalf("batch name = %q, want growth", b.Name)
	}
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

func TestLoomyGrowthBatchIsAPlannerAndTaskProvider(t *testing.T) {
	var c *Client
	_ = core.BatchPlanner(c)
	_ = core.TaskProvider(c)
}

func TestLoomyPlannedBatchesIncludeTheDailyCheckin(t *testing.T) {
	c := &Client{}
	batches := core.PlannedBatches(c)
	if len(batches) != 2 {
		t.Fatalf("PlannedBatches = %+v, want growth plus checkin", batches)
	}
	if batches[0].Name != "growth" || batches[1].Name != core.CheckinBatchName {
		t.Fatalf("PlannedBatches = %+v, want growth then %q", batches, core.CheckinBatchName)
	}
	if !core.IsCheckinBatch(c, batches[1]) {
		t.Fatal("the scheduled Loomy check-in was not recognised as the synthetic check-in batch")
	}
}
