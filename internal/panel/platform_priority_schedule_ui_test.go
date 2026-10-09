package panel

import (
	"strings"
	"testing"
)

func TestPlatformsPriorityScheduleEditor(t *testing.T) {
	src := poolStatsUISource(t)

	render := poolStatsFuncBody(t, src, "renderPlatforms")
	for _, want := range []string{
		`priority_schedule`,
		`pfSchedToggle`,
		`pfSchedCount`,
		`pfPriorityScheduleHTML`,
	} {
		if !strings.Contains(render, want) {
			t.Errorf("renderPlatforms is missing %q", want)
		}
	}

	save := poolStatsFuncBody(t, src, "savePlatforms")
	for _, want := range []string{
		`pfReadPrioritySchedule`,
		`priority_schedule`,
	} {
		if !strings.Contains(save, want) {
			t.Errorf("savePlatforms is missing %q", want)
		}
	}

	read := poolStatsFuncBody(t, src, "pfReadPrioritySchedule")
	for _, want := range []string{
		`pfClockMinute`,
		`pfPriorityWindowsOverlap`,
		`pfSchedStart`,
		`pfSchedEnd`,
		`pfSchedPrio`,
		`must differ`,
		`overlap`,
	} {
		if !strings.Contains(read, want) {
			t.Errorf("pfReadPrioritySchedule is missing %q", want)
		}
	}

	body := poolStatsFuncBody(t, src, "pfSyncPrioritySchedule")
	for _, want := range []string{
		`pfClockMinute`,
		`pfPriorityWindowContains`,
		`pfSchedNow`,
		`pfSchedRow`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pfSyncPrioritySchedule is missing %q", want)
		}
	}

	for _, want := range []string{
		`ev.target.closest(".pfSchedToggle")`,
		`ev.target.closest(".pfSchedAdd")`,
		`ev.target.closest(".pfSchedDel")`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("platform schedule editor has no event branch for %q", want)
		}
	}
}
