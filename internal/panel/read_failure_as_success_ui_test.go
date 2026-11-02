package panel

import (
	"strings"
	"testing"
)

// 任务中心「读失败被当成成功」一类（issue #121 + #122）。
//
// 两单同型，而且都出在任务中心那排「扫描」上：一次读真的失败了，界面却给出
// 成功的结论——
//   - #121：`qcScan` 读 `POST <client>/tasks/scan_all` 失败时，把「空组 + 庆祝
//     标题」一起传给 `renderQueue`，于是失败态复用真·空态的「没有待办任务 🎉」，
//     只有副文案换成英文报错。操作员点扫描是为了知道还有没有活要干，把「读不到」
//     画成「都做完了」会直接导致当天该领的任务不领。
//   - #122：`tcScan` 里 `renderSchedule()`（async）既没 await 也没接 catch，
//     而 `toast("已重新读取排期","ok")` 是无条件的，于是「请求已失败」和「已经
//     重新读取」在同一帧出口，且绿色 toast 是屏幕上更亮、还会自己消失的那个。
//
// 判据不是「函数长什么样」，而是「失败那次调用有没有一条不同的出口」：
// 失败必须走自己的渲染/提示分支，不能滑进成功分支或与成功共用同一个标题。

// TestAFailedScanIsNotRenderedAsTheAllDoneEmptyState 钉住 #121。
func TestAFailedScanIsNotRenderedAsTheAllDoneEmptyState(t *testing.T) {
	src := poolStatsUISource(t)

	scan := poolStatsFuncBody(t, src, "qcScan")
	// 失败分支必须走专门的错误渲染，而不是复用空态标题。
	if !strings.Contains(scan, "renderQueueError(") {
		t.Fatal("qcScan 的失败分支没有走 renderQueueError：失败会复用「没有待办任务 🎉」的庆祝空态（issue #121）")
	}
	// 回归钉子：失败分支里不许再出现「把空组 + 庆祝标题」的旧写法。
	if strings.Contains(scan, `renderQueue([], null, "没有待办任务`) {
		t.Error("qcScan 失败分支仍在复用成功空态标题：读失败被画成「都做完了」（issue #121 回归）")
	}
	// 失败要有一句可见的提示；错误文本必须过 esc（它来自上游，可能带标记）。
	if !strings.Contains(scan, `toast("扫描失败`) {
		t.Error("qcScan 失败时没有 toast：只有一小行副文案，操作员会当成空态（issue #121）")
	}

	// 错误渲染器本身：必须是独立的标题，不许写庆祝文。
	err := poolStatsFuncBody(t, src, "renderQueueError")
	if !strings.Contains(err, "扫描失败") {
		t.Error("renderQueueError 的标题不是「扫描失败」：失败与空态又共用一个标题了（issue #121）")
	}
	if strings.Contains(err, "🎉") {
		t.Error("renderQueueError 里出现庆祝语气：读失败不能报喜（issue #121）")
	}
	// 它必须清掉上一轮的待办列表，否则「读失败」下面还挂着旧结果。
	if !strings.Contains(err, `$("#qcList")`) {
		t.Error("renderQueueError 没有清空待办列表：会与上一轮结果混在一起（issue #121）")
	}

	// 按钮文案必须复位到点名前的原文：静态名是「扫描成长任务」，复位值不能是
	// 另一个名字（点一次入口就改名，下次找不到）。
	if !strings.Contains(scan, "const keep = b.textContent") || !strings.Contains(scan, "b.textContent = keep") {
		t.Error("qcScan 复位按钮文案时没记住原文：静态名「扫描成长任务」会被改成别的（issue #121）")
	}
	if strings.Contains(scan, `b.textContent = "扫描待办"`) {
		t.Error("qcScan 仍把按钮名硬编码成「扫描待办」：与静态入口名不一致（issue #121）")
	}
	// 静态名必须真的在 HTML 里（否则上面的「原文」是空的）。
	if !strings.Contains(src, `<button type="button" id="btnScanAll">扫描成长任务</button>`) {
		t.Error("找不到 #btnScanAll 的静态名「扫描成长任务」：复位文案没有可回归的原文（issue #121）")
	}
}

// TestTheScheduleScanReportsTheRealOutcome 钉住 #122。
func TestTheScheduleScanReportsTheRealOutcome(t *testing.T) {
	src := poolStatsUISource(t)

	// renderSchedule 必须把结果交出来，调用方才可能按结果说话。
	render := poolStatsFuncBody(t, src, "renderSchedule")
	if !strings.Contains(render, "return false") || !strings.Contains(render, "return true") {
		t.Error("renderSchedule 没有成功/失败返回值：调用方无从知道这次读成没成（issue #122）")
	}

	scan := poolStatsFuncBody(t, src, "tcScan")
	// 必须 await，否则结果拿到手是 undefined。
	if !strings.Contains(scan, "await renderSchedule()") {
		t.Fatal("tcScan 没有 await renderSchedule()：结果拿不到，成功提示必然无条件（issue #122）")
	}
	// 成功提示必须叼在结果上：两种结论都要出现，且 err 那支要有 err 配色。
	if !strings.Contains(scan, "重新读取排期失败") {
		t.Error("tcScan 没有失败提示：读失败时不会报出来（issue #122）")
	}
	if !strings.Contains(scan, `"err"`) {
		t.Error("tcScan 的失败分支没有用 err 配色：失败与成功长得一样（issue #122）")
	}
	// 回归钉子：不许再出现「先 renderSchedule 后无条件 toast ok」的旧写法。
	if strings.Contains(scan, `renderSchedule();\n  toast("已重新读取排期", "ok");`) {
		t.Error("tcScan 仍在无条件弹成功提示（issue #122 回归）")
	}
	// 在途反馈：与同页那颗「刷新看板任务状态」同一套口径。
	for _, want := range []string{`b.disabled = true`, `b.textContent = "读取中…"`, "b.disabled = false"} {
		if !strings.Contains(scan, want) {
			t.Errorf("tcScan 缺少在途/收尾语句 %q：点了看不出在读（issue #122）", want)
		}
	}
	// 静态按钮名要在（复位值取的是它）。
	if !strings.Contains(src, `id="btnTcScan"`) {
		t.Error("找不到 #btnTcScan")
	}
}
