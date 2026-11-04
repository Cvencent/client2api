package panel

import (
	"strings"
	"testing"
)

// 钉住两处动效层修复（issue #190 / #191）。

// #190：入场动画的 fill 模式不得把 transform/opacity 永久钉在终态——
// animation 来源的层叠高于一切作者声明（含内联 style 与 :hover），`both`
// 演完就占位，hover 抬升、按下下沉、3D 倾斜、柱子点亮全部失效。
func TestEntryAnimationsDoNotPinTheFinalState(t *testing.T) {
	src := poolStatsUISource(t)

	// 积分卡 pkIn：backwards + 关键帧只管 from（to 的 transform:none 曾把
	// :hover 的 -1px 永久压住）。
	if strings.Contains(src, "pkIn 240ms var(--ease-out) both") {
		t.Error("pkIn 仍用 fill both：终态会钉死 transform，:hover/:active 全部失效（issue #190）")
	}
	pkAt := strings.Index(src, "@keyframes pkIn {")
	if pkAt >= 0 {
		pkIn := src[pkAt : pkAt+strings.Index(src[pkAt:], "}")+1]
		if strings.Contains(pkIn, "to {") {
			t.Errorf("pkIn 关键帧仍写 to 帧：%s（终态应落基础声明，issue #190）", pkIn)
		}
	}

	// 柱子 uscolIn：同理，to 的 opacity .82 曾把 :hover/.on 的 opacity:1 压住。
	if strings.Contains(src, "uscolIn 460ms cubic-bezier(.16,1,.3,1) both") {
		t.Error("uscolIn 仍用 fill both：柱子只亮 filter 不亮本体（issue #190）")
	}
	ucAt := strings.Index(src, "@keyframes uscolIn {")
	if ucAt >= 0 {
		uscolIn := src[ucAt : ucAt+strings.Index(src[ucAt:], "}")+1]
		if strings.Contains(uscolIn, "to {") {
			t.Errorf("uscolIn 关键帧仍写 to 帧：%s（.82 在基础规则里，issue #190）", uscolIn)
		}
	}

	// fxCascade 演完撤类（fxStaggerRows 范式）：.fx-in 常驻曾把 initTilt 写
	// 的内联 transform 压成恒等矩阵，进页头几秒 3D 倾斜不跟手。
	fx := poolStatsFuncBody(t, src, "fxCascade")
	if !strings.Contains(fx, `classList.remove("fx-in")`) || !strings.Contains(fx, "animationend") {
		t.Fatal("fxCascade 没有在 animationend 撤 .fx-in：入场 fill 会永久压住内联 transform（issue #190）")
	}
	if !strings.Contains(fx, `{ once: true }`) {
		t.Error("fxCascade 的 animationend 监听没有 once：反复重挂监听会泄漏（issue #190）")
	}
}

// #191：零消费者的机制不许留——.fx-lazy / data-fx-grow / @keyframes fxIn
// 曾是「CSS、JS、注释、CHANGELOG 全齐、markup 零命中」的死机制，每进一页
// 白跑两次全子树扫描，还会误导后来者按注释去用。
func TestFxMechanismsHaveConsumers(t *testing.T) {
	// 注释里的删除历史会提到这些名字，先剥注释再断言——只对「活代码」把关。
	src := stripPanelComments(poolStatsUISource(t))

	for _, dead := range []string{".fx-lazy", "data-fx-grow", "@keyframes fxIn ", "fxScrollIn"} {
		if strings.Contains(src, dead) {
			t.Errorf("死机制 %q 还在源码里：零消费者的机制要么接上消费者要么整链删除（issue #191）", dead)
		}
	}
	// fxGrow 的关键帧必须保留（.pk-expiry-fill 是静态消费者）。
	if !strings.Contains(src, "@keyframes fxGrow") || !strings.Contains(src, "animation: fxGrow 640ms") {
		t.Fatal("fxGrow 关键帧或其静态消费者丢了：到期条的生长动画失效")
	}
	// fxCascade 还在编排（.fx-in 是活的）。
	if !strings.Contains(src, "fxCascade(host)") {
		t.Fatal("fxCascade 调用丢了：入场级联失效")
	}
}
