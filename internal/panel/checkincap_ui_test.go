package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 签到能力位拆分的界面回归测试。
//
// 背景：core.Capabilities 里原先只有 checkin 一位，它等于 len(Actions) > 0。
// 七个 CheckinActions 实现里有六个会按「当前存在的账号」收窄自己——codearts
// 没有凭据时返回空、minimaxcode 凭据过期时返回空——于是这两个模块在「客户端
// 能力矩阵」的每日签到列被渲染成「-」。操作员看到的就是「这个客户端你没做签到」，
// 而它们其实都实现了签到，只是此刻没有账号可以点。
//
// 现在 core 把两个问题拆成两位：checkin（客户端级——这个模块有没有签到能力）
// 和 checkin_ready（行级——现在有没有按钮可以点）。界面必须各读各的：
//   · 能力矩阵 renderClients 读 checkin，和它读 tasks / refresh_models 一样；
//   · 行内按钮 accRowHTML 读 checkin_ready；
//   · 客户端级「全部签到」bulk 与批量扫描 scanCheckin 读 checkin。
//
// 这些全是 index.html 里的字符串拼接，Go 编译器管不到，所以静态钉住。
// ---------------------------------------------------------------------------

// checkinCapWiring 返回界面里读错签到能力位的地方。返回空切片表示全对。
//
// 返回列表而不是就地断言，是为了让下面两个负控能用同一段代码证明它真的会红。
func checkinCapWiring(t *testing.T, src string) []string {
	t.Helper()
	var miss []string
	row := poolStatsFuncBody(t, src, "accRowHTML")
	matrix := poolStatsFuncBody(t, src, "renderClients")
	bulkFn := poolStatsFuncBody(t, src, "bulk")
	scan := poolStatsFuncBody(t, src, "scanCheckin")

	// 1. 行内按钮必须读行级位：模块报了签到能力、但此刻没有可用账号时，
	//    按钮要消失，而不是点下去才失败。
	if !strings.Contains(row, "if (caps.checkin_ready) {") {
		miss = append(miss, "accRowHTML 没有按行级的 checkin_ready 渲染签到按钮")
	}
	if strings.Contains(row, "if (caps.checkin) {") {
		miss = append(miss, "accRowHTML 仍在按客户端级的 checkin 渲染签到按钮")
	}

	// 2. 能力矩阵必须读客户端级位：它问的是「这个模块有没有签到」。
	if !strings.Contains(matrix, "yn(k.checkin)") {
		miss = append(miss, "renderClients 的每日签到列没有读客户端级的 checkin")
	}
	if strings.Contains(matrix, "yn(k.checkin_ready)") {
		miss = append(miss, "renderClients 的每日签到列读了行级的 checkin_ready：没有账号的模块会显示成「-」")
	}

	// 3. 客户端级入口读客户端级位：有签到能力就去试，而不是按「现在有没有按钮」拒绝。
	if !strings.Contains(bulkFn, `if (!caps.checkin) return toast(`) {
		miss = append(miss, "bulk 的签到分支没有按客户端级的 checkin 判断")
	}
	if !strings.Contains(scan, "if (!capsOf(n).checkin) return;") {
		miss = append(miss, "scanCheckin 没有按客户端级的 checkin 筛选模块")
	}
	return miss
}

func TestCheckinCapabilityBitsAreReadWhereTheyBelong(t *testing.T) {
	if miss := checkinCapWiring(t, poolStatsUISource(t)); len(miss) > 0 {
		t.Fatalf("签到能力位读错了：\n  - %s", strings.Join(miss, "\n  - "))
	}
}

// TestCheckinCapWiringCatchesARowButtonOnTheClientLevelBit 是负控：把行内按钮
// 改回客户端级位，检查器必须报出来——否则上面那条断言可能只是在自证。
func TestCheckinCapWiringCatchesARowButtonOnTheClientLevelBit(t *testing.T) {
	src := poolStatsUISource(t)
	const want = "if (caps.checkin_ready) {"
	if !strings.Contains(src, want) {
		t.Fatalf("测试自己失效了：index.html 里找不到 %q", want)
	}
	if miss := checkinCapWiring(t, strings.Replace(src, want, "if (caps.checkin) {", 1)); len(miss) == 0 {
		t.Fatal("把行内按钮改回客户端级的 checkin 之后，检查器还是绿的")
	}
}

// TestCheckinCapWiringCatchesAMatrixOnTheRowLevelBit 是第二个负控：把能力矩阵
// 换成行级位，检查器必须报出来。
func TestCheckinCapWiringCatchesAMatrixOnTheRowLevelBit(t *testing.T) {
	src := poolStatsUISource(t)
	const want = "yn(k.checkin)"
	if !strings.Contains(src, want) {
		t.Fatalf("测试自己失效了：index.html 里找不到 %q", want)
	}
	if miss := checkinCapWiring(t, strings.Replace(src, want, "yn(k.checkin_ready)", 1)); len(miss) == 0 {
		t.Fatal("把能力矩阵换成行级的 checkin_ready 之后，检查器还是绿的")
	}
}
