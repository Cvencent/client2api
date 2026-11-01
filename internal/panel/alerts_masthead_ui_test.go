package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 告警页页头的时序回归（issue #49）。
//
// mountMasthead 是 go() 进页那一刻同步算的，而 ALERT_ROWS 是 renderAlerts
// 异步回来的——冷启动进告警页时它还是空数组，「未读」磁贴的副文案走
// 「干净」分支，和同屏十几条流水自相矛盾（#8 的分档被时序绕过）。#27 在
// models 页修的是同一类竞态：数据落地后 if (VIEW === ...) mountMasthead(...)
// 重算一次。这里钉住告警页也有同一个补丁，且别把补丁挂错地方。
// ---------------------------------------------------------------------------

// TestRenderAlertsRecomputesTheMastheadAfterRowsLand 要求 renderAlerts 在
// ALERT_ROWS 赋值之后重算当前页页头，且重算被守在 VIEW === "alerts" 之下——
// 告警数据的刷新也发生在别的页面驻留时，不能给不相关视图重挂页头。
func TestRenderAlertsRecomputesTheMastheadAfterRowsLand(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderAlerts")

	if !strings.Contains(body, "ALERT_ROWS = rows") {
		t.Fatal("renderAlerts 不再给 ALERT_ROWS 赋值：本测试和 3D 舞台的数据契约都失效了")
	}
	assign := strings.Index(body, "ALERT_ROWS = rows")
	recompute := strings.Index(body, `mountMasthead("alerts")`)
	if recompute < 0 {
		t.Fatal("renderAlerts 没有在流水落地后重算页头：冷启动进告警页副文案会停在「干净」（issue #49）")
	}
	if recompute < assign {
		t.Fatal("页头重算发生在 ALERT_ROWS 赋值之前：重算时读到的还是旧数组，副文案依旧和流水打架")
	}
	// 守卫必须和重算在同一处：无条件重算会把页头挂到别的视图上。
	guard := `if (VIEW === "alerts") mountMasthead("alerts");`
	if !strings.Contains(body, guard) {
		t.Fatal(`renderAlerts 的页头重算没有 if (VIEW === "alerts") 守卫：后台刷新会给别的视图重挂页头`)
	}

	// 对照组：#27 在 models 页的同类补丁必须仍然在（防有人误删一处以为两处共用）。
	mbody := poolStatsFuncBody(t, src, "renderModels")
	if !strings.Contains(mbody, `if (VIEW === "models") mountMasthead("models");`) {
		t.Fatal("renderModels 的页头重算（issue #27）消失了：模型页冷启动页头会停回 0/0")
	}
}

// TestMastheadSubCopyKeepsTheFlowTier 钉住 #8 的分档本身还在：有未读报「需要
// 看一眼」，没未读但有流水报条数，都没有才是「干净」。时序补丁是在给这个
// 分档补「首屏也能走到」的路径，分档被改掉的话补丁就没有意义了。
func TestMastheadSubCopyKeepsTheFlowTier(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, `n ? "需要看一眼" : (flow ? flow + " 条流水" : "干净")`) {
		t.Fatal("页头副文案的 #8 分档不在了：有流水报条数的行为（issue #8）被改掉")
	}
	if !strings.Contains(src, `const flow = (ALERT_ROWS || []).length;`) {
		t.Fatal("副文案读的不是 ALERT_ROWS：流水条数来源变了，「N 条流水」会失真")
	}
}
