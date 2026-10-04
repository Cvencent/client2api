package core

import "testing"

// PlannedBatches is the bridge between the panel's per-account check-in button
// and the task centre's scheduled batches.  These tests pin the two shapes it
// must keep apart: a module that declares its own "checkin" batch means task
// codes to run, while a check-in-only module gets a synthetic one.

func TestPlannedBatchesPrefersADeclaredPlanner(t *testing.T) {
	c := &planningClient{plainClient{name: "pl"}}
	bs := PlannedBatches(c)
	if len(bs) != 1 || bs[0].Name != "checkin" {
		t.Fatalf("PlannedBatches = %+v, want the module's declared checkin batch", bs)
	}
	if IsCheckinBatch(c, bs[0]) {
		t.Fatal("IsCheckinBatch = true for a module that declares its own checkin batch")
	}
}

func TestPlannedBatchesSynthesizesCheckinForACheckinOnlyModule(t *testing.T) {
	c := &checkinStub{plainClient: plainClient{name: "ci"}, actions: []CheckinAction{{ID: "daily"}}}
	bs := PlannedBatches(c)
	if len(bs) != 1 || bs[0].Name != CheckinBatchName {
		t.Fatalf("PlannedBatches = %+v, want one %q batch", bs, CheckinBatchName)
	}
	if len(bs[0].Codes) != 1 || bs[0].Codes[0] != CheckinBatchName {
		t.Fatalf("synthetic batch codes = %v, want [%s]", bs[0].Codes, CheckinBatchName)
	}
	if !IsCheckinBatch(c, bs[0]) {
		t.Fatal("IsCheckinBatch = false for the synthetic check-in batch")
	}
	if got, ok := BatchOf(c, CheckinBatchName); !ok || got.Name != CheckinBatchName {
		t.Fatalf("BatchOf = %+v/%v, want the synthetic check-in batch", got, ok)
	}
}

func TestPlannedBatchesIgnoresModulesWithNoBatchCapability(t *testing.T) {
	if bs := PlannedBatches(&plainClient{name: "plain"}); len(bs) != 0 {
		t.Fatalf("PlannedBatches = %+v, want none", bs)
	}
}
