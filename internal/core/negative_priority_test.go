package core

import "testing"

func TestResolveNegativePlatformPriorityUsesSmallestNumberFirst(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "shared-model"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "shared-model"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: -5},
		"beta":  {Priority: -10},
	})

	gotClient, gotModel := resolveOK(t, r, "shared-model")
	if gotClient != "beta" || gotModel != "shared-model" {
		t.Fatalf("Resolve = (%q, %q), want beta because -10 is higher priority than -5", gotClient, gotModel)
	}
}

func TestAccountPriorityNegativeValuesAreRankedNumerically(t *testing.T) {
	SetAccountPriorities(map[string]map[string]int{
		"workbuddy": {
			"minus-five":   -5,
			"minus-ten":    -10,
			"zero-default": 0,
		},
	})
	t.Cleanup(func() { SetAccountPriorities(nil) })

	type row struct{ id string }
	rows := []row{{"zero-default"}, {"minus-five"}, {"minus-ten"}}
	got := LowestPriorityTier("workbuddy", rows, func(r row) string { return r.id })
	if len(got) != 1 || got[0].id != "minus-ten" {
		t.Fatalf("LowestPriorityTier = %+v, want only minus-ten", got)
	}
}
