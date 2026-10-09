package main

import (
	"testing"

	"client2api/internal/scheduler"
)

// The daily balance refresh defaults to once a day inside the 00:00-00:30 CST
// window: that is when the platforms that grant a daily quota top accounts up,
// so one early-morning sweep is what turns "the vendor gave me credits" into a
// usable, uncooled account without waiting for an operator to press refresh.

func TestScheduleDailyBalanceDefaultsAndOverrides(t *testing.T) {
	cfg := fileConfig{Schedule: scheduleConfig{Enabled: true}}
	sc := cfg.schedule()
	if !sc.DailyBalance.Enabled || len(sc.DailyBalance.Hours) != 1 || sc.DailyBalance.Hours[0] != 0 {
		t.Fatalf("default daily balance = %+v, want enabled at hour 0", sc.DailyBalance)
	}

	off := false
	cfg = fileConfig{Schedule: scheduleConfig{Enabled: true, DailyBalanceEnabled: &off}}
	if sc := cfg.schedule(); sc.DailyBalance.Enabled {
		t.Fatalf("explicit daily_balance_enabled=false was ignored: %+v", sc.DailyBalance)
	}

	// An explicit empty hour list disables it even with the switch on, the same
	// way an empty hour list means "never" for every other batch.
	cfg = fileConfig{Schedule: scheduleConfig{Enabled: true, DailyBalanceHours: []int{}}}
	if sc := cfg.schedule(); sc.DailyBalance.Enabled {
		t.Fatalf("explicit empty daily_balance_hours was ignored: %+v", sc.DailyBalance)
	}

	// A partial per-platform override inherits the shared switch and only
	// replaces the hours it names.
	cfg = fileConfig{Schedule: scheduleConfig{
		Enabled: true,
		Clients: map[string]map[string]scheduleOverride{
			"loomy": {"daily_balance": {Hours: []int{1}}},
		},
	}}
	got := cfg.schedule().Clients["loomy"][scheduler.DailyBalanceTaskName]
	if !got.Enabled || len(got.Hours) != 1 || got.Hours[0] != 1 {
		t.Fatalf("loomy daily_balance override = %+v, want enabled hour 1", got)
	}
}
