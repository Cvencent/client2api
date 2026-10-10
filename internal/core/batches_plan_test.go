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

type growthAndCheckinPlanner struct {
	checkinStub
}

func (c *growthAndCheckinPlanner) Batches() []Batch {
	return []Batch{{Name: "growth", Codes: []string{"onboarding"}}}
}

type declaredCheckinAndGrowthPlanner struct {
	checkinStub
}

func (c *declaredCheckinAndGrowthPlanner) Batches() []Batch {
	return []Batch{
		{Name: "growth", Codes: []string{"onboarding"}},
		{Name: CheckinBatchName, Codes: []string{"claim"}},
	}
}

func TestPlannedBatchesAddsSyntheticCheckinBesideOtherDeclaredBatches(t *testing.T) {
	c := &growthAndCheckinPlanner{checkinStub{
		plainClient: plainClient{name: "loomy"},
		actions:     []CheckinAction{{ID: "daily"}},
	}}

	bs := PlannedBatches(c)
	if len(bs) != 2 {
		t.Fatalf("PlannedBatches = %+v, want the declared growth batch plus synthetic checkin", bs)
	}
	if bs[0].Name != "growth" || bs[1].Name != CheckinBatchName {
		t.Fatalf("PlannedBatches = %+v, want growth then checkin", bs)
	}
	if IsCheckinBatch(c, bs[0]) {
		t.Fatal("IsCheckinBatch = true for the declared growth batch")
	}
	if !IsCheckinBatch(c, bs[1]) {
		t.Fatal("IsCheckinBatch = false for the synthetic check-in batch")
	}
}

func TestPlannedBatchesDoesNotDuplicateADeclaredCheckinBatch(t *testing.T) {
	c := &declaredCheckinAndGrowthPlanner{checkinStub{
		plainClient: plainClient{name: "declared"},
		actions:     []CheckinAction{{ID: "claim"}},
	}}

	bs := PlannedBatches(c)
	if len(bs) != 2 {
		t.Fatalf("PlannedBatches = %+v, want exactly the two declared batches", bs)
	}
	if IsCheckinBatch(c, bs[1]) {
		t.Fatal("IsCheckinBatch = true for a module-declared checkin batch")
	}
}
