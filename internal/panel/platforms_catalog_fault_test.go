package panel

import (
	"strings"
	"testing"
)

// 钉住「模型目录读失败时，平台页不得把空渲染当真、保存不得静默删配置」
// （issue #180，P1）。
//
// renderPlatforms 曾经只对 cfg 判失败：/panel/api/models 这一半读失败（或
// 该平台这次缺席响应）时，卡片画成「0 个模型 · 模块未上报目录」——一句不实
// 陈述；随后 savePlatforms 拿 0 行的表单减 baseline，把已有的每模型优先级
// 逐个写成 null、黑名单清空，汇报成功。配置没有撤销/历史，丢就是丢了。
func TestPlatformsCatalogFaultStopsTheSave(t *testing.T) {
	src := poolStatsUISource(t)

	// 1) md 读失败与 cfg 同等对待：画错误块并 return，不渲染 0 模型。
	render := poolStatsFuncBody(t, src, "renderPlatforms")
	if !strings.Contains(render, "if (!md.ok)") {
		t.Fatal("renderPlatforms 没有对 /panel/api/models 的失败分支：目录读不到会渲染成「0 个模型」（issue #180）")
	}
	mdAt := strings.Index(render, "if (!md.ok)")
	mdBranch := render[mdAt:]
	if ret := strings.Index(mdBranch, "return;"); ret < 0 || ret > 600 {
		t.Fatalf("md 失败分支没有就近 return（offset %d）：会继续走空目录渲染", ret)
	} else if !strings.Contains(mdBranch[:ret], "读取失败") {
		t.Error("md 失败分支必须明说「读取失败」，不能冒充空目录")
	}

	// 2) 「这次没读到」与「没上报目录」在数据上可区分：clientModels.error
	//    与缺键都要落进 catalogFaults，空数组（真没上报）不算。
	if !strings.Contains(render, "catalogFaults") {
		t.Fatal("renderPlatforms 没有记录 catalogFaults：故障与合法空态无从区分（issue #180）")
	}
	if !strings.Contains(render, "x.error") {
		t.Error("目录响应的 clientModels.error 没被消费：模块上报错误时会静默渲染成空清单")
	}

	// 3) savePlatforms 在有 fault 时拒绝保存。
	save := poolStatsFuncBody(t, src, "savePlatforms")
	if !strings.Contains(save, "catalogFaults") {
		t.Fatal("savePlatforms 不看 catalogFaults：0 行 diff 会把 baseline 全写成 null（issue #180）")
	}
	if !strings.Contains(save, "已拒绝保存") {
		t.Error("拒绝保存时必须说清原因与下一步（重新进页），不能只报失败")
	}

	// 4) 卡片的空态提示要区分：fault 说故障，无 fault 才说「模块未上报目录」。
	if !strings.Contains(src, "模型目录读取失败：") || !strings.Contains(src, "模块未上报目录") {
		t.Error("卡片的空态文案没有区分「读取故障」与「未上报目录」两种形态（issue #180）")
	}
}
