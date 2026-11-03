package panel

import (
	"regexp"
	"strings"
	"testing"
)

// 面板块头部的下拉菜单必须点得到；积分读数不许在数字中间折行（issue #123 + #124）。
//
// 两单同属「面板 CSS 布局」一类，而且都属于**文本断言拦不住**的那一类：
//   - #123：菜单渲染完全正常（文字、分组、分隔线都在），只有真实鼠标命中测试
//     才暴露「每一项都落到菜单下面的看板/表格上」——键盘 Tab 反而正常，所以自测
//     发现不了。几何判据在 cmd/panelsmoke 的 assertLayout（真浏览器）。
//   - #124：九位数在逗号后被 overflow-wrap:anywhere 断开，读成两个数。
//
// 这里钉的是**会漂移的契约**：层叠关系、裁剪放行、以及「表头与数据行共用同一套
// 列模板」——最后一列只改一处会让两行错位。

// TestTheBoxHeaderSitsAboveItsSiblings 钉住 #123 的层叠契约。
//
// `.box > *` 给每个直接子元素上了 `position: relative; z-index: 1`，于是
// <header> 自成层叠上下文，菜单自己的 z-index 跳不出去，被 DOM 上排在其后的
// 看板/表格整片盖住。header 必须在此之上，且这条必须**后写**（同权重时后写者胜）。
func TestTheBoxHeaderSitsAboveItsSiblings(t *testing.T) {
	src := poolStatsUISource(t)
	style := panelStyleBlock(t, src)

	// 找出 `.box > *` 那条的 z-index，与 `.box > header` 的 z-index 比较。
	star := regexp.MustCompile(`\.box > \* \{[^}]*z-index:\s*(\d+)`).FindStringSubmatch(style)
	if star == nil {
		t.Fatal("找不到 `.box > *` 的 z-index：柔光层之下这条仍在，否则本契约无从谈起")
	}
	hdr := regexp.MustCompile(`\.box > header \{[^}]*z-index:\s*(\d+)`).FindStringSubmatch(style)
	if hdr == nil {
		t.Fatal("`.box > header` 没有抬高层级：菜单会被后续兄弟盖住，鼠标点不动（issue #123）")
	}
	if !(hdr[1] > star[1]) {
		t.Errorf("`.box > header` 的 z-index=%s 不高于 `.box > *` 的 %s：菜单仍然跳不出 header 的层叠上下文（issue #123）",
			hdr[1], star[1])
	}
	// 后写才生效：同权重下先写的那条会被覆盖。
	if strings.Index(style, ".box > header {") < strings.Index(style, ".box > * {") {
		t.Error("`.box > header` 的层级规则写在 `.box > *` 之前：同权重会被后者覆盖，等于没改（issue #123）")
	}
}

// TestAnOpenMenuIsNotClippedByTheBox 钉住 #123 的第二条路径。
func TestAnOpenMenuIsNotClippedByTheBox(t *testing.T) {
	style := panelStyleBlock(t, poolStatsUISource(t))
	if !strings.Contains(style, ".box:has(.opmore[open])") {
		t.Fatal("没有「菜单展开时放开 .box 的裁切」这条规则：盒子矮时菜单下半截被切掉、点不到（issue #123）")
	}
	// 必须是放开裁切，而不是把 .box 永久改成 visible——那会让上面的 sticky 保存条
	// 失去参照（.box 的 overflow:clip 正是为了这个才选的，见该处注释）。
	m := regexp.MustCompile(`\.box:has\(\.opmore\[open\]\) \{\s*overflow:\s*(\w+)`).FindStringSubmatch(style)
	if m == nil || m[1] != "visible" {
		t.Fatalf(".box:has(.opmore[open]) 没有把 overflow 放开成 visible（got %v）", m)
	}
}

// TestTheExpiryReadingNeverBreaksMidNumber 钉住 #124。
func TestTheExpiryReadingNeverBreaksMidNumber(t *testing.T) {
	src := poolStatsUISource(t)

	// 读数本身禁止折行。
	row := regexp.MustCompile(`\.pk-expiry-row > b \{[^}]*\}`).FindString(src)
	if row == "" {
		t.Fatal("找不到 .pk-expiry-row > b 的规则")
	}
	if !strings.Contains(row, "white-space: nowrap") {
		t.Error("积分读数没有 white-space: nowrap：九位数会在数字中间折成两行（issue #124）")
	}
	if strings.Contains(row, "overflow-wrap: anywhere") {
		t.Error("积分读数仍有 overflow-wrap: anywhere：它正是「在逗号后断开」的原因（issue #124）")
	}

	// 表头与数据行必须共用同一套列模板：只改一处会让两行错位。最后一列还要能
	// 容纳九位数（下限不低于原来的 72px）。
	tpls := regexp.MustCompile(`\.pk-expiry-(?:hdr|row) \{\s*display: grid; grid-template-columns: ([^;]+);`).FindAllStringSubmatch(src, -1)
	if len(tpls) != 2 {
		t.Fatalf("期望找到 hdr/row 两条列模板，实际 %d 条", len(tpls))
	}
	if strings.TrimSpace(tpls[0][1]) != strings.TrimSpace(tpls[1][1]) {
		t.Errorf("表头与数据行的列模板不一致：%q vs %q（会错位，issue #124）", tpls[0][1], tpls[1][1])
	}
	last := tpls[0][1]
	if strings.Contains(last, " 72px;") || strings.HasSuffix(strings.TrimSpace(last), "72px") {
		t.Error("最后一列仍是固定 72px：九位数自然宽约 80px，装不下就会折行（issue #124）")
	}
}
