package core

import (
	"testing"
	"time"
)

func TestPlatformPriorityScheduleOverridesBasePriority(t *testing.T) {
	r := NewRegistry()
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {
			Priority: 9,
			PrioritySchedule: []PriorityWindow{
				{StartMinute: 1 * 60, EndMinute: 5 * 60, Priority: 1},
				{StartMinute: 5 * 60, EndMinute: 8 * 60, Priority: -2},
			},
		},
	})

	cst := time.FixedZone("CST", 8*60*60)
	cases := []struct {
		hour, minute, want int
	}{
		{0, 59, 9},
		{1, 0, 1},
		{4, 59, 1},
		{5, 0, -2},
		{7, 59, -2},
		{8, 0, 9},
	}
	for _, tc := range cases {
		at := time.Date(2026, 10, 9, tc.hour, tc.minute, 0, 0, cst)
		if got := r.priorityAt("alpha", at); got != tc.want {
			t.Errorf("priorityAt(%02d:%02d) = %d, want %d", tc.hour, tc.minute, got, tc.want)
		}
	}
}

func TestPlatformPriorityScheduleSupportsCrossMidnight(t *testing.T) {
	r := NewRegistry()
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {
			Priority: 7,
			PrioritySchedule: []PriorityWindow{
				{StartMinute: 23 * 60, EndMinute: 6 * 60, Priority: -10},
			},
		},
	})

	cst := time.FixedZone("CST", 8*60*60)
	cases := []struct {
		hour, minute, want int
	}{
		{22, 59, 7},
		{23, 0, -10},
		{23, 59, -10},
		{0, 0, -10},
		{5, 59, -10},
		{6, 0, 7},
	}
	for _, tc := range cases {
		at := time.Date(2026, 10, 9, tc.hour, tc.minute, 0, 0, cst)
		if got := r.priorityAt("alpha", at); got != tc.want {
			t.Errorf("priorityAt(%02d:%02d) = %d, want %d", tc.hour, tc.minute, got, tc.want)
		}
	}
}
