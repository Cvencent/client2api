package panel

import (
	"strings"
	"testing"
)

// 账号池轮询那一拍的读几何时机（issue #165）。
//
// refreshAll() 里 renderAccounts() 之后紧挨着一个同步 markScrollableTables()：
// 整表 DOM 刚被替换，浏览器还没为新布局算过任何东西，此刻读 scrollWidth 是
// 强制同步布局——200 个账号 233ms，占那一拍的六成，且与用户操作争主线程。
// 修法两条：轮询路径改成 rAF（scheduleScrollableCheck，让浏览器先落布局再读），
// 函数本体先攒读再统一写、跳过隐藏容器。

func TestPollPathDoesNotReadGeometrySynchronously(t *testing.T) {
	src := poolStatsUISource(t)

	// refreshAll 里的调用必须是 scheduleScrollableCheck（rAF 一帧后读），
	// 不再是同步 markScrollableTables()。
	i := strings.Index(src, "async function refreshAll(")
	if i < 0 {
		t.Fatal("refreshAll 不在：看守失去锚点（issue #165）")
	}
	j := strings.Index(src[i:], "\nfunction ")
	if j < 0 {
		j = len(src) - i
	}
	seg := src[i : i+j]
	if !strings.Contains(seg, "scheduleScrollableCheck();") {
		t.Error("refreshAll 没有把读几何挪到下一帧：轮询仍会在整表重建的同一个任务里强制同步布局（issue #165）")
	}
	if strings.Contains(seg, "markScrollableTables();") {
		t.Error("refreshAll 仍在同步调 markScrollableTables：重建与布局串行压在同一任务（issue #165）")
	}

	// scheduleScrollableCheck 必须存在且走 requestAnimationFrame。
	k := strings.Index(src, "function scheduleScrollableCheck()")
	if k < 0 {
		t.Fatal("scheduleScrollableCheck 不在：rAF 通道没了（issue #165）")
	}
	if !strings.Contains(src[k:k+220], "requestAnimationFrame(markScrollableTables)") {
		t.Error("scheduleScrollableCheck 没有走 requestAnimationFrame：读几何回到同步路径（issue #165）")
	}
}

func TestMarkScrollableTablesBatchesReadsBeforeWrites(t *testing.T) {
	src := stripLineComments(stripPanelComments(poolStatsUISource(t)))

	i := strings.Index(src, "function markScrollableTables()")
	if i < 0 {
		t.Fatal("markScrollableTables 不在：看守失去锚点（issue #165）")
	}
	j := strings.Index(src[i:], "\nwindow.addEventListener")
	if j < 0 {
		j = len(src) - i
	}
	body := src[i : i+j]

	// 函数体必须是「先攒读（scrollWidth/clientWidth/scrollLeft 一次性读进数组）、
	// 再统一写 class」：forEach 里读写交替会迫使浏览器对每个容器各做一次布局。
	if !strings.Contains(body, "scrollWidth - w.clientWidth - w.scrollLeft") {
		t.Error("markScrollableTables 不再批量读几何：读被拆散后每个容器各触发一次强制布局（issue #165）")
	}
	// toggle 必须发生在攒读之后：body 里 toggle 的第一次出现位置晚于几何读取。
	read := strings.Index(body, "scrollWidth")
	toggle := strings.Index(body, "classList.toggle")
	if read < 0 || toggle < 0 || toggle < read {
		t.Error("markScrollableTables 的写先于读：读写交替回来了（issue #165）")
	}
	// 不得回到逐条 forEach 读写交替的旧形态。
	if strings.Contains(body, ".forEach(w =>") {
		t.Error("markScrollableTables 回到 forEach 逐条形态：那是读写交替的旧写法（issue #165）")
	}
}
