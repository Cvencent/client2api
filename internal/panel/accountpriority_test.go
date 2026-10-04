package panel

import (
	"testing"

	"client2api/internal/core"
)

func TestDecorateAccountPrioritiesReadsTheRegistry(t *testing.T) {
	r := core.NewRegistry()
	r.SetPlatformConfigs(map[string]core.PlatformConfig{
		"workbuddy": {AccountPriorities: map[string]int{"a1": -3, "a2": 7}},
	})
	t.Cleanup(func() { core.SetAccountPriorities(nil) })
	p := &panel{opts: Options{Registry: r}}
	list := []core.AccountRecord{{ID: "a1"}, {ID: "a2"}, {ID: "a3"}}
	p.decorateAccountPriorities("workbuddy", list)

	if list[0].Priority != -3 || list[1].Priority != 7 || list[2].Priority != 0 {
		t.Fatalf("priorities = %d/%d/%d, want -3/7/0",
			list[0].Priority, list[1].Priority, list[2].Priority)
	}
}
