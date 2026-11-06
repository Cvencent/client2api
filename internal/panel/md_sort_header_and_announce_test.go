package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 模型页排序表头的视觉状态（issue #227）与异步读取结论的读屏通道（#228）。
//
// #227：mdContextSortToggle 只写 aria-sort 与 data-md-sort-dir——读屏读到
// ascending/descending，明眼用户看到的一直是"未排序"的 ↕；data-md-sort-dir
// 在 CSS 里零消费。修法照账号表 syncAccSortHeader 的既有形态：sort-on class
// + ↑/↓ 字形，排序样式的作用域从 #view-accounts 放宽到所有 th.sortable。
//
// #228：「重新获取」的成败只写进无播报的 #mdNote（liveCount=0）。修法：
// #mdNote 挂 role=status（计数变化礼貌播报），失败路径补一次 role=alert 的
// toast（异常结论打断播报）；成功不 toast，避免每次刷新刷屏。
// ---------------------------------------------------------------------------

// TestModelSortHeaderHasVisibleState 钉住 #227 的两个接线点与样式作用域。
func TestModelSortHeaderHasVisibleState(t *testing.T) {
	src := poolStatsUISource(t)

	// 同步函数必须存在，且 class/字形/aria 三者都写。
	helper := poolStatsFuncBody(t, src, "syncMdSortHeader")
	if !strings.Contains(helper, `classList.toggle("sort-on"`) {
		t.Error("syncMdSortHeader 不写 sort-on class：命中列的强调色永远不出现（issue #227）")
	}
	if !strings.Contains(helper, `"↑"`) || !strings.Contains(helper, `"↓"`) {
		t.Error("syncMdSortHeader 不写方向字形：箭头永远停在未排序的 ↕（issue #227）")
	}
	if !strings.Contains(helper, `setAttribute("aria-sort"`) {
		t.Error("syncMdSortHeader 不写 aria-sort：读屏与视觉两个消费者必须同时满足（issue #227）")
	}

	// 两个调用点：toggle（点击/键盘）与 renderModels（进场同步）。
	toggle := poolStatsFuncBody(t, src, "mdContextSortToggle")
	if !strings.Contains(toggle, "syncMdSortHeader()") {
		t.Error("mdContextSortToggle 不走 syncMdSortHeader：点击排序后表头仍然不动（issue #227）")
	}
	if !strings.Contains(src, "syncMdSortHeader(); // aria + class + 箭头一处同步") {
		t.Error("renderModels 的同步处没有换用 syncMdSortHeader：进场态与点击态两套行为（issue #227）")
	}

	// 样式作用域必须已放宽：th.sortable.sort-on 不能还锁在 #view-accounts 里
	// （否则模型表加了 class 也没有视觉）。
	if strings.Contains(src, "#view-accounts table.acc th.sortable.sort-on") {
		t.Error("sort-on 样式仍锁在 #view-accounts：模型表的 sort-on 加了也白加（issue #227）")
	}
	if !strings.Contains(src, "th.sortable.sort-on { color: var(--accent-text); }") {
		t.Error("共享的 sort-on 规则不见了（issue #227）")
	}
	// 账号表的既有锚（#87 一族钉过的列宽等）不受影响：普通列宽规则仍在。
	if !strings.Contains(src, "#view-accounts table.acc th.sortable { cursor: pointer;") &&
		!strings.Contains(src, "th.sortable { cursor: pointer;") {
		t.Error("th.sortable 的 cursor/transition 基础规则不见了（issue #227 回归）")
	}
}

// TestModelsReadFailureIsAnnounced 钉住 #228：失败结论必须既有 note 又有
// role=alert 的 toast；#mdNote 本身必须是 role=status。成功路径不得 toast
// （每次刷新都打断 = 刷屏），但计数更新要靠 #mdNote 的 role=status 礼貌播报。
func TestModelsReadFailureIsAnnounced(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `<span class="note" id="mdNote" role="status">`) {
		t.Fatal("#mdNote 没有 role=status：手动刷新后的计数变化读屏听不到（issue #228）")
	}
	render := poolStatsFuncBody(t, src, "renderModels")
	if !strings.Contains(render, `toast("获取模型目录失败：`) {
		t.Error("模型目录读失败不 toast：读屏用户按了「重新获取」听不到任何结论（issue #228）")
	}
	if !strings.Contains(render, `"获取失败："`) {
		t.Error("读失败的 note 文案不见了：失败原因失去落点（issue #228）")
	}
	// renderModels 里 toast 只许出现在异常结论上：读失败一条 + 既有的
	// 「部分行来自回退列表」告警一条，都必须带 "err"；成功是常态，打断播报
	// 等于刷屏（issue #228）。
	n := strings.Count(render, "toast(")
	if n < 1 || n > 2 {
		t.Errorf("renderModels 里 toast 出现 %d 次（期望 1~2：读失败 + 既有回退告警；成功也 toast 会刷屏，issue #228）", n)
	}
	for _, ln := range strings.Split(render, "\n") {
		if strings.Contains(ln, "toast(") && !strings.Contains(ln, `"err"`) {
			t.Errorf("renderModels 里出现了不带 err 的 toast：%s（成功结论不该打断播报，issue #228）", strings.TrimSpace(ln))
		}
	}
}
