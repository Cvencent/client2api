package core

import "testing"

func TestAccountPriorityTierPrefersLowestValue(t *testing.T) {
	SetAccountPriorities(map[string]map[string]int{
		"workbuddy": {
			"a": 5,
			"b": -1,
			"c": -1,
			"d": 0,
		},
	})
	t.Cleanup(func() { SetAccountPriorities(nil) })

	if got := AccountPriority("workbuddy", "b"); got != -1 {
		t.Fatalf("AccountPriority(b) = %d, want -1", got)
	}
	if got := AccountPriority("workbuddy", "missing"); got != 0 {
		t.Fatalf("AccountPriority(missing) = %d, want the zero default", got)
	}

	type row struct{ id string }
	rows := []row{{"a"}, {"b"}, {"c"}, {"d"}}
	got := LowestPriorityTier("workbuddy", rows, func(r row) string { return r.id })
	if len(got) != 2 || got[0].id != "b" || got[1].id != "c" {
		t.Fatalf("tier = %+v, want b/c in original order", got)
	}
}
