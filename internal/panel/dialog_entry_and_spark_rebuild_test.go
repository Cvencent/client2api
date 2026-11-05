package panel

import (
	"strings"
	"testing"
)

// 钉住弹窗进场唯一来源（issue #206）与火花「关→开」重建（issue #205）。

// #206：`.dlg` 的进场 animation 曾有四条同权重声明（dlgSpring / dlgIn×2 /
// uiDlgIn）分居四段，源码顺序让最早的弹簧一次都没播过。现在只有一条权威
// 声明（dlgSpring，接回过冲弹簧），同名 keyframes 与死关键帧清零。
func TestDialogEntryHasOneAuthoritativeAnimation(t *testing.T) {
	src := poolStatsUISource(t)

	// .dlg 上带 animation 的声明必须唯一，且是 dlgSpring（.veil.out .dlg 的
	// 退场是另一属性场景，不算）。
	count := strings.Count(src, "\n.dlg { animation:")
	if count != 1 {
		t.Errorf(".dlg 上的进场 animation 声明出现 %d 次，必须唯一（issue #206，曾四条同权重抢同一属性）", count)
	}
	if !strings.Contains(src, ".dlg { animation: dlgSpring 260ms cubic-bezier(.2, 1.1, .35, 1) both; }") {
		t.Fatal("权威声明不是 dlgSpring：过冲弹簧被顶掉的老问题回来了（issue #206）")
	}
	// 同名 keyframes 与死关键帧清零。
	if strings.Contains(src, "@keyframes dlgIn") || strings.Contains(src, "@keyframes uiDlgIn") {
		t.Error("同名/死 keyframes 还在源码里：靠源码顺序决定谁生效是最难查的一种（issue #206）")
	}
	// dlgSpring 的关键帧本体在（声明所引用的）。
	if !strings.Contains(src, "@keyframes dlgSpring") {
		t.Fatal("dlgSpring 关键帧丢了：声明引用不存在的动画")
	}
}

// #205：initSpark 把偏好判断放在创建层，冷启动在 reduce 下不建 canvas 也
// 不挂监听——之后关掉偏好整场不回来。onReduceChange 的「离开 reduce」分支
// 必须补建（与 bgfStart 并列），且 initSpark 的早退仍保留幂等。
func TestSparkIsRebuiltWhenLeavingReduce(t *testing.T) {
	src := poolStatsUISource(t)
	handlerAt := strings.Index(src, "const onReduceChange = () => {")
	if handlerAt < 0 {
		t.Fatal("找不到 onReduceChange")
	}
	handlerEnd := strings.Index(src[handlerAt:], "};")
	handler := src[handlerAt : handlerAt+handlerEnd]
	elseAt := strings.Index(handler, "else if (typeof bgfStart === \"function\") bgfStart();")
	if elseAt < 0 {
		t.Fatal("onReduceChange 没有离开 reduce 的 else 分支")
	}
	tail := handler[elseAt:]
	if !strings.Contains(tail, "initSpark()") {
		t.Fatal("离开 reduce 的分支没有补建火花：冷启动在 reduce 下关掉偏好后火花整场缺席（issue #205）")
	}
	// initSpark 幂等早退仍在（SPARK.canvas 已存在就返回），创建层的偏好抑制
	// 交给补建而不是改早退。
	init := poolStatsFuncBody(t, src, "initSpark")
	if !strings.Contains(init, "SPARK.canvas") {
		t.Error("initSpark 的幂等守卫不见了：重复调用会重建 canvas")
	}
}
