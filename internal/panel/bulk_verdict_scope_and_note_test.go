package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 批量测试的收尾结论（issue #233）与进度承载面（#234）。
//
// #157 把 const failures = [] 放进 try 块，读取点在 try/finally 之后同一
// 函数体——块作用域越界让收尾必然抛 ReferenceError，分档 toast 自落地起
// 一次都没执行过；panelBootError 的「首错即止」又让这条噪音把同会话后续
// 所有异常一起静音。#182 让菜单选中即收起，而 bulk("test") 的进度只写在
// 被收起的按钮上——整轮探测在屏幕上不可见。
// ---------------------------------------------------------------------------

// TestBulkTestFailuresInScopeAndVerdictReachesTheNote 钉住 #233 与 #234：
// failures 必须声明在 (async () => { 的函数体里（与读取点同作用域）；
// 进行中进度与收尾结论都必须写 #accNote（菜单收起后的常驻承载面）。
func TestBulkTestFailuresInScopeAndVerdictReachesTheNote(t *testing.T) {
	src := poolStatsUISource(t)

	// failures 声明不许在 try 块内：块内 const + 块后读取 = 必然越界。
	if strings.Contains(src, "      try {\n        const failures = [];") {
		t.Fatal("failures 仍在 try 块内声明：收尾读取必然 ReferenceError（issue #233）")
	}
	// 声明必须出现在 (async () => { 之前、bulk 函数体内。
	seg := src[strings.Index(src, `let done = 0, ok = 0, failed = 0;`):]
	segEnd := strings.Index(seg, "} else if (kind === \"checkin\")")
	if segEnd < 0 {
		t.Fatal("bulk 的 test 分支失去锚点")
	}
	seg = seg[:segEnd]
	if !strings.Contains(seg, "const failures = [];") {
		t.Error("failures 不在 bulk 的 test 分支函数体里：作用域仍未对齐（issue #233）")
	}
	if !strings.Contains(seg, "(async () => {\n      try {") {
		t.Error("test 分支的 async IIFE 形状变了：看守失去锚点（issue #233）")
	}

	// #234：进度与结论走 #accNote（progress 闭包 + 收尾 verdict）。
	if !strings.Contains(seg, `const progress = (text) => {`) {
		t.Error("bulk test 没有 progress 闭包：菜单收起后整轮进度不可见（issue #234）")
	}
	if !strings.Contains(seg, `note.textContent = text;`) {
		t.Error("progress 不写 #accNote：承载面没接上（issue #234）")
	}
	if !strings.Contains(seg, "progress(verdict);") {
		t.Error("收尾结论没有落 #accNote：toast 消失后无处可查（issue #234）")
	}
	if strings.Count(seg, "progress(") < 3 {
		t.Errorf("progress 的调用点只有 %d 处（期望 ≥3：进度×2 + 收尾），中途某几拍仍不可见（issue #234）", strings.Count(seg, "progress("))
	}
}

// TestPanelBootErrorUpdatesInsteadOfFreezing 钉住 #233 的放大器：已有红条时
// 必须更新文案并计次，不许「首错即止」把后续异常全部静音。
func TestPanelBootErrorUpdatesInsteadOfFreezing(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "panelBootError")

	if strings.Contains(body, "if (document.documentElement.dataset.c2aBootError) return;") {
		t.Fatal("panelBootError 仍是首错即止：一条噪音静音整个会话的后续异常（issue #233）")
	}
	if !strings.Contains(body, `getElementById("c2aBootError")`) {
		t.Error("panelBootError 不查找已有红条：更新逻辑没接上（issue #233）")
	}
	if !strings.Contains(body, "本会话第") {
		t.Error("红条更新不带次数：首错与后续错误不可区分（issue #233）")
	}
}
