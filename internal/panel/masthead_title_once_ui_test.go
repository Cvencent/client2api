package panel

import (
	"strings"
	"testing"
)

// 页头标题动画的回归（issue #90）。
//
// mountMasthead 挂在 5 秒轮询路径上；它每调用一次都把主标题重拆成一排带
// from opacity:0 入场动画的 span——新元素 = 动画从零开始，标题整行每 5 秒
// 反复重播。修复：拆分只在首建时做一次（childElementCount 守卫），后续
// 轮询只更新右侧读数；reduce 用户保持 textContent 路径。

func TestMastheadTitleIsBuiltOnceNotPerPoll(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "mountMasthead")

	if !strings.Contains(body, "else if (!titleEl.childElementCount)") {
		t.Fatal("mountMasthead 的标题拆分没有 childElementCount 守卫：每 5 秒轮询都会重拆 span 并重播逐个淡入（issue #90）")
	}
	// 拆分必须还在（首建时要做动画），但要在守卫之内：childElementCount
	// 的判定必须先于 innerHTML 重拆出现。
	if !strings.Contains(body, "titleEl.innerHTML = chars.map") {
		t.Fatal("mountMasthead 丢了标题拆分动画本身：进页不再有逐个淡入")
	}
	guard := strings.Index(body, "else if (!titleEl.childElementCount)")
	build := strings.Index(body, "titleEl.innerHTML = chars.map")
	if guard < 0 || build < 0 || build < guard {
		t.Fatal("标题拆分不在首建守卫之内：轮询路径仍会重拆（issue #90）")
	}
}
