package panel

import (
	"strings"
	"testing"
)

// 跳过链接对比度（issue #102）与能力矩阵 AX 两态（issue #100）的回归。

// TestSkipLinkColorFollowsTheTheme 钉住 #102：写死的 #fff 在深色块的
// --accent（#4c7dff）上只有 3.69:1；--accent-ink 随主题配过，两个主题都达标。
func TestSkipLinkColorFollowsTheTheme(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, "background: var(--accent); color: var(--accent-ink); font: 550 13px var(--sans);") {
		t.Fatal("跳过链接的写死 #fff 还在：深色主题下对比度 3.69:1，键盘用户的第一个焦点读不清（issue #102）")
	}
}

// TestCapabilityCellsSpeakBothStates 钉住 #100 的两半：
// 每格 aria-label 写明「有：X / 无：X」，表头行不再整行 aria-hidden。
func TestCapabilityCellsSpeakBothStates(t *testing.T) {
	src := poolStatsUISource(t)
	cap := poolStatsFuncBody(t, src, "berthCap")

	if !strings.Contains(cap, `aria-label="' + esc((on ? "有：" : "无：") + (label || "")) + '"'`) && !strings.Contains(cap, `aria-label="' + esc((on ? "有：" : "无：")`) {
		t.Fatal("能力格没有 aria-label 两态：「管」和「·」在 AX 里是同一个节点，读屏两态念一样的（issue #100）")
	}
	if !strings.Contains(cap, "有：") || !strings.Contains(cap, "无：") {
		t.Fatal("能力格没有 aria-label 两态：「管」和「·」在 AX 里是同一个节点，读屏两态念一样的（issue #100）")
	}
	hdr := poolStatsFuncBody(t, src, "renderClients")
	if strings.Contains(hdr, `b-head" aria-hidden="true"`) {
		t.Fatal("能力表头仍整行 aria-hidden：列名是格子的唯一上下文，藏掉它 AX 只剩七枚同名节点（issue #100）")
	}
	if !strings.Contains(hdr, `aria-label="' + esc(cap + "（" + ab + "）")`) {
		t.Fatal("表头能力列没有给 AX 真名（issue #100）")
	}
}
