package panel

import (
	"strings"
	"testing"
)

// 对话页 live region 的时序回归（issue #76）。
//
// #chatLog 是 aria-live="polite" 的整块容器，而流式回复期间每收到一个 SSE
// 分片就整段重写 innerHTML——读屏软件把「容器变了」理解为「重新播报整段」，
// 41 个分片实测 44 次播报、每次都是全量历史。修复：chatDraw 带 streaming
// 参数，流式中间态把 aria-live 摘到 off，settle 的最终绘制恢复 polite，
// 让完成的回复作为一次宣告落地。

// TestChatDrawSuspendsTheLiveRegionWhileStreaming 要求 chatDraw 的函数体：
// 1. 接受 streaming 参数并按它切换 #chatLog 的 aria-live；
// 2. 流式时置 off（读屏闭嘴），最终绘制恢复 polite（一次宣告）。
func TestChatDrawSuspendsTheLiveRegionWhileStreaming(t *testing.T) {
	src := poolStatsUISource(t)
	draw := poolStatsFuncBody(t, src, "chatDraw")
	if !strings.Contains(draw, `live.setAttribute("aria-live", streaming ? "off" : "polite")`) {
		t.Fatal("chatDraw 没有按 streaming 参数切换 #chatLog 的 aria-live：流式期间读屏会逐片重播整段历史（issue #76）")
	}
	// 流式分片的调用点必须带 true（中间态闭嘴），settle 的最终绘制必须带
	// false（完成时一次宣告）——缺了后者，live region 会永远停在 off，
	// 整页回复都不播报，比原来更糟。
	if !strings.Contains(src, "chatDraw(true)") {
		t.Fatal("流式分片没有以 streaming=true 调 chatDraw：中间态仍然逐片宣告（issue #76）")
	}
	if !strings.Contains(src, "chatDraw(false)") {
		t.Fatal("最终绘制没有以 streaming=false 调 chatDraw：aria-live 会永远停在 off，完成的回复不再被播报（issue #76）")
	}
}
