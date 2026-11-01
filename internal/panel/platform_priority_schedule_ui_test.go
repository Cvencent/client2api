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
	// 徽标数的是「有内容的行」而不是 DOM 行数（issue #80）：无规则时渲染层
	// 故意塞一行空占位，按行数数出来恒显「1 条」，和同屏的「当前未命中分时
	// 规则」互相打脸。空行判据渲染层自己就有，格式无效的行也不算——它连保存
	// 都会被拒，计数只对能存下来的规则负责；0 条显示「未配置」。
	for _, want := range []string{
		`if (!empty && !bad) realRows++;`,
		`realRows === 0 ? "未配置" : realRows + " 条"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pfSyncPrioritySchedule 的徽标没有按有内容行计数（issue #80）：缺少 %q", want)
		}
	}
	if strings.Contains(body, `badge.textContent = rows.length + " 条"`) {
		t.Error("pfSyncPrioritySchedule 仍在按 DOM 行数计徽标：空占位行会被算成一条规则（issue #80）")
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
