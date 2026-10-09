package main

import (
	"reflect"
	"testing"
	"time"

	"client2api/internal/scheduler"
)

func TestScheduleRecoveryDefaultsAndOverrides(t *testing.T) {
	cfg := fileConfig{Schedule: scheduleConfig{Enabled: true}}
	sc := cfg.schedule()
	if !sc.Recovery.Enabled || sc.Recovery.Every != 4*time.Hour || sc.Recovery.Jitter != time.Hour {
		t.Fatalf("default recovery = %+v, want enabled 4h +/-1h", sc.Recovery)
	}

	off := false
	cfg = fileConfig{Schedule: scheduleConfig{Enabled: true, RecoveryEnabled: &off}}
	if sc := cfg.schedule(); sc.Recovery.Enabled {
		t.Fatalf("explicit recovery_enabled=false was ignored: %+v", sc.Recovery)
	}

	on := true
	every, jitter := 120, 30
	cfg = fileConfig{Schedule: scheduleConfig{
		Enabled: true,
		Clients: map[string]map[string]scheduleOverride{
			"loomy": {"recovery": {Enabled: &off, EveryMinutes: &every, JitterMinutes: &jitter}},
			"other": {"recovery": {Enabled: &on}},
		},
	}}
	sc = cfg.schedule()
	loomy := sc.Clients["loomy"][scheduler.RecoveryTaskName]
	if loomy.Enabled || loomy.Every != 2*time.Hour || loomy.Jitter != 30*time.Minute {
		t.Fatalf("loomy recovery override = %+v, want disabled 2h +/-30m", loomy)
	}
	other := sc.Clients["other"][scheduler.RecoveryTaskName]
	if !other.Enabled || other.Every != 4*time.Hour || other.Jitter != time.Hour {
		t.Fatalf("partial recovery override = %+v, want inherited 4h +/-1h", other)
	}
	if sc.Recovery.Every != 4*time.Hour || sc.Recovery.Jitter != time.Hour {
		t.Fatalf("per-platform override mutated shared recovery: %+v", sc.Recovery)
	}
}

func TestScheduleRecoveryExplicitZeroJitterStaysZero(t *testing.T) {
	every, jitter := 120, 0
	cfg := fileConfig{Schedule: scheduleConfig{
		Enabled: true,
		Clients: map[string]map[string]scheduleOverride{
			"loomy": {"recovery": {EveryMinutes: &every, JitterMinutes: &jitter}},
		},
	}}
	got := cfg.schedule().Clients["loomy"][scheduler.RecoveryTaskName]
	if !got.Enabled || got.Every != 2*time.Hour || got.Jitter != 0 {
		t.Fatalf("explicit zero jitter = %+v, want inherited enabled with 2h and no jitter", got)
	}
}

// TestScheduleProjectsPerClientOverrides pins the config-file -> scheduler
// projection of schedule.clients.  Three shapes have three different meanings,
// and collapsing any two of them would silently change what the operator asked
// for:
//
//   - {hours: [7, 19]}        an override that runs then
//   - {enabled: false}        this platform never runs this batch
//   - {}                      not an override at all, follow the shared group
func TestScheduleProjectsPerClientOverrides(t *testing.T) {
	off := false
	cfg := fileConfig{Schedule: scheduleConfig{
		Enabled:      true,
		CheckinHours: []int{9},
		Clients: map[string]map[string]scheduleOverride{
			"wb": {
				"checkin":  {Hours: []int{7, 19}},
				"travel":   {Enabled: &off},
				"growth":   {Hours: []int{}},
				"activity": {},
			},
		},
	}}

	sc := cfg.schedule()
	if !sc.Enabled {
		t.Fatal("the master switch was dropped")
	}
	wb := sc.Clients["wb"]
	if wb == nil {
		t.Fatalf("schedule.clients.wb was dropped entirely: %+v", sc.Clients)
	}

	if g, ok := wb["checkin"]; !ok || !g.Enabled || !reflect.DeepEqual(g.Hours, []int{7, 19}) {
		t.Errorf("checkin override = %+v/%v, want enabled [7 19]", g, ok)
	}
	if g, ok := wb["travel"]; !ok || g.Enabled {
		t.Errorf("travel override = %+v/%v, want present and disabled", g, ok)
	}
	if g, ok := wb["growth"]; !ok || g.Enabled || len(g.Hours) != 0 {
		t.Errorf("growth override = %+v/%v, want an explicit never-run entry", g, ok)
	}
	if _, ok := wb["activity"]; ok {
		t.Error("an override with neither a switch nor hours must not be materialised")
	}

	// The shared group is untouched by any of this.
	if g, ok := sc.Shared("checkin"); !ok || !reflect.DeepEqual(g.Hours, []int{9}) {
		t.Errorf("shared checkin = %+v/%v, want the untouched [9]", g, ok)
	}
	// The scheduler must read the override for wb and the shared group for a
	// platform with no entry.
	if g, ok := sc.GroupFor("wb", "checkin"); !ok || !reflect.DeepEqual(g.Hours, []int{7, 19}) {
		t.Errorf("GroupFor(wb) = %+v/%v, want the override", g, ok)
	}
	if g, ok := sc.GroupFor("other", "checkin"); !ok || !reflect.DeepEqual(g.Hours, []int{9}) {
		t.Errorf("GroupFor(other) = %+v/%v, want the shared group", g, ok)
	}
}

// TestScheduleWithoutClientsKeepsTheSharedTimetable is the migration case: an
// existing config has no schedule.clients key at all, and must keep behaving
// exactly as before.
func TestScheduleWithoutClientsKeepsTheSharedTimetable(t *testing.T) {
	cfg := fileConfig{Schedule: scheduleConfig{Enabled: true, CheckinHours: []int{9}}}
	sc := cfg.schedule()
	if len(sc.Clients) != 0 {
		t.Fatalf("Clients = %v, want none", sc.Clients)
	}
	if g, ok := sc.GroupFor("wb", "checkin"); !ok || !g.Enabled || !reflect.DeepEqual(g.Hours, []int{9}) {
		t.Fatalf("GroupFor = %+v/%v, want the shared 09:00", g, ok)
	}
	// The panel's "follow the shared hours" display reads Groups(), which must
	// still list every batch even when no override exists.
	if got := len(sc.Groups()); got == 0 {
		t.Fatal("Groups() = empty, want the six shared batches")
	}
	if _, ok := sc.Override("wb", "checkin"); ok {
		t.Fatal("Override reported an entry for a config that has none")
	}
}

func TestScheduleProjectsAccountScope(t *testing.T) {
	cfg := fileConfig{Schedule: scheduleConfig{
		Enabled: true,
		Clients: map[string]map[string]scheduleOverride{
			"loomy": {
				"growth": {
					Hours: []int{12},
					Accounts: &scheduleAccountScope{
						Mode:    scheduler.AccountScopeExclude,
						Exclude: []string{"a2"},
					},
				},
			},
		},
	}}

	sc := cfg.schedule()
	g := sc.Clients["loomy"]["growth"]
	if g.Accounts.Mode != scheduler.AccountScopeExclude || !reflect.DeepEqual(g.Accounts.Exclude, []string{"a2"}) {
		t.Fatalf("account scope = %+v, want exclude a2", g.Accounts)
	}
}
