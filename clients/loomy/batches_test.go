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
