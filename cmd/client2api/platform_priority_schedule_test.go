package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"client2api/internal/core"
)

// A platform can override its base priority during configured local-time
// windows. The projection normalises HH:MM to minutes once; a malformed
// window is dropped rather than taking the whole platform down.
func TestPlatformConfigsProjectsPrioritySchedule(t *testing.T) {
	cfg := &fileConfig{Platforms: map[string]platformConfig{
		"zcode": {
			Priority: 5,
			PrioritySchedule: []priorityWindowConfig{
				{Start: "01:00", End: "05:00", Priority: 1},
				{Start: "05:00", End: "08:00", Priority: -2},
				{Start: "25:00", End: "26:00", Priority: 3},
			},
		},
	}}

	got := cfg.platformConfigs()["zcode"]
	want := []core.PriorityWindow{
		{StartMinute: 60, EndMinute: 300, Priority: 1},
		{StartMinute: 300, EndMinute: 480, Priority: -2},
	}
	if !reflect.DeepEqual(got.PrioritySchedule, want) {
		t.Fatalf("priority schedule = %#v, want %#v", got.PrioritySchedule, want)
	}

	var parsed fileConfig
	if err := json.Unmarshal([]byte(`{"platforms":{"zcode":{"priority_schedule":[{"start":"23:00","end":"06:00","priority":-1}]}}}`), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Platforms["zcode"].PrioritySchedule) != 1 {
		t.Fatalf("priority_schedule did not round-trip: %+v", parsed.Platforms["zcode"])
	}
}
