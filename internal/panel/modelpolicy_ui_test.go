package panel

import (
	"strings"
	"testing"
)

// modelpolicy_ui_test.go 钉住「模型」弹层的可编辑模型策略接线。它是纯字符串
// 拼接的界面代码，Go 编译器管不到；少一个 data-* 或事件绑定，弹层会照常
// 打开却保存不了任何东西，而且浏览器不报错。
//
// 覆盖三件事：
//   1. 弹层按 capabilities.model_policy 决定是否可编辑，并渲染复选框/强制启用；
//   2. 保存走 POST /accounts/<id>/model-policy，body 里的黑名单取「取消勾选」的项；
//   3. 默认全勾选（没有 disabled_models 的账号画出来就是全开）。

func TestModelDialogShowsPolicyControls(t *testing.T) {
	src := poolStatsUISource(t)
	dialog := poolStatsFuncBody(t, src, "openModelsDialog")
	cell := poolStatsFuncBody(t, src, "modelCellHTML")
	for _, want := range []string{
		`.model_policy`,      // 能力开关来自 capabilities
		`mparkForce`,         // 强制启用开关
		`强制启用`,               // 操作员看得到的文案
		`data-mpark-all="1"`, // 全选
		`data-mpark-all="0"`, // 全不选
	} {
		if !strings.Contains(dialog, want) {
			t.Errorf("openModelsDialog 缺少 %q：弹层里没有这个控件", want)
		}
	}
	if !strings.Contains(cell, `mparkBox`) {
		t.Error("modelCellHTML 缺少 mparkBox：弹层里没有逐模型复选框")
	}
	if !strings.Contains(dialog, `editable`) {
		t.Error("弹层没有区分可编辑与只读两种模式")
	}
}

func TestModelDialogCheckboxesDefaultToAllowed(t *testing.T) {
	src := poolStatsUISource(t)
	// 复选框只在「被操作员关掉」时不勾选；其余一律 checked，即默认全开。
	cell := poolStatsFuncBody(t, src, "modelCellHTML")
	if !strings.Contains(cell, `(disabled ? "" : " checked")`) {
		t.Error("模型复选框不是在未禁用时默认勾选：默认应当全部开启")
	}
	// 弹层把 park/黑名单之外的目录项也算进来，保证全开账号能画满、能全勾。
	dialog := poolStatsFuncBody(t, src, "openModelsDialog")
	if !strings.Contains(dialog, "sum.catalog.slice()") {
		t.Error("弹层没有以平台目录为基准：默认全开的账号可能画不全")
	}
}

func TestModelPolicySavePostsTheBlacklist(t *testing.T) {
	src := poolStatsUISource(t)
	svc := poolStatsFuncBody(t, src, "saveModelsPolicy")
	if !strings.Contains(svc, `"/model-policy"`) {
		t.Error("保存没有打到 /accounts/<id>/model-policy")
	}
	for _, want := range []string{
		`disabled_models:`, // 黑名单字段
		`force_enabled:`,   // 强制启用字段
		`method: "POST"`,
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("saveModelsPolicy 缺少 %q", want)
		}
	}
	// 未勾选 = 关闭 = 进入黑名单。
	if !strings.Contains(svc, `!box.checked`) {
		t.Error("保存时没有把「取消勾选」翻译成 disabled_models")
	}
	// 目录里看不到、但磁盘上仍被关掉的模型必须保留，不能被静默重新打开。
	if !strings.Contains(svc, "MODEL_POLICY.original") {
		t.Error("保存会丢掉仍然存在但目录里看不到的 disabled_models 条目")
	}
	if !strings.Contains(src, `$("#btnModelSave").addEventListener("click", saveModelsPolicy)`) {
		t.Error("保存按钮没有接线")
	}
}
