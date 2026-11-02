package panel

import (
	"regexp"
	"strings"
	"testing"
)

// 「统一机制/先例已经在面板里存在，但一整片消费者没接上」一类（issue #74 + #50）。
//
// 两单都不是「某处写错了」，而是同一件事只做成了一部分：
//   - #74：字号单位全是 px——`font-size` 385 条、125 条 `font` 简写里 114 条也带
//     px 字号、四个 `--type-*` 令牌与 6 处 `clamp()` 上下界同样是 px。浏览器
//     「只放大文字」这条路径因此整站失效（WCAG 2.2 AA 1.4.4）；
//   - #50：配置页 32 个控件没有 dirty 追踪，而排程页早就有一套
//     （scHasDirty / scUpdateDirty + beforeunload 拦截）。
//
// 判据因此是「这一片消费者是不是都在同一条机制上」，不是单个声明写没写对。

// pxFontRE 匹配仍是绝对像素的字号声明：`font-size: 12px`、`font: 600 10px/1 …`。
var pxFontRE = regexp.MustCompile(`(?:font-size\s*:\s*|(?:\bfont\s*:\s*)(?:[^;}\n]*?\s)?)[\d.]+px`)

// TestEveryFontSizeIsRelative 钉住 #74：面板里不许再有绝对像素字号。
//
// 这条不是「样式洁癖」：字号写死 px 时，「只放大文字」这类辅助设置（浏览器/
// 系统的默认字号、Firefox「仅缩放文本」）整站无效——文字一个像素都不动。
// 1rem = 基准 16px（html 上设 font-size: 100%），所以迁移是纯单位换算，
// 默认档位下每个字号与原值逐像素相等。
func TestEveryFontSizeIsRelative(t *testing.T) {
	src := poolStatsUISource(t)

	// 基准必须显式写在 html 上：没有它，rem 会退回浏览器默认值，「只放大文字」
	// 改的是同一个值所以仍能生效，但显式声明让「1rem = 16px」成为面板自己的契约。
	if !strings.Contains(src, "html { font-size: 100%;") {
		t.Fatal("html 上没有字号基准 font-size: 100%：rem 依赖的基准不是面板自己声明的（issue #74）")
	}

	css := panelStyleBlock(t, src)

	// ① font-size / font 简写都不许再带 px 字号。简写这一片是 #74 里最容易漏的：
	// 面板绝大多数读数与状态词走的是 `font: 600 10.5px/1 var(--mono)`，只改
	// font-size 会整片漏掉。
	if bad := pxFontRE.FindAllString(css, -1); len(bad) > 0 {
		t.Errorf("样式表里仍有 %d 处绝对像素字号，例如 %q：字号应当用 rem（issue #74）", len(bad), bad[0])
	}
	// ② --type-* 令牌是字号的第二处来源，一并要有 rem。
	for _, tok := range []string{"--type-xs", "--type-md", "--type-base", "--type-xl"} {
		re := regexp.MustCompile(regexp.QuoteMeta(tok) + `\s*:\s*([^;]+)`)
		m := re.FindStringSubmatch(css)
		if m == nil {
			t.Errorf("找不到字号令牌 %s（issue #74）", tok)
			continue
		}
		if !strings.Contains(m[1], "rem") {
			t.Errorf("%s = %q 仍是绝对单位：改基准字号时它不动（issue #74）", tok, strings.TrimSpace(m[1]))
		}
	}
	// ③ clamp() 的上下界也是字号（大屏标题那几处）：px 会让缩放对它们失效。
	for _, c := range regexp.MustCompile(`clamp\([^)]*\)`).FindAllString(css, -1) {
		if strings.Contains(c, "px") {
			t.Errorf("clamp(%s) 的边界是绝对像素：这类字号不随基准缩放（issue #74）", c)
		}
	}
	// ④ rem 迁移必须是主体的形状，不能只改两条就宣称完成。
	if n := strings.Count(css, "rem"); n < 300 {
		t.Errorf("样式表里 rem 只出现 %d 次：迁移没覆盖主体（issue #74）", n)
	}
	// ⑤ 页面内联的字号（JS 拼的 style 属性）同样要相对化。
	if strings.Contains(src, "font-size:11.5px") {
		t.Error("仍有一处内联 font-size:11.5px 写在 JS 里：那片文字不随基准缩放（issue #74）")
	}
}

// TestConfigPageTracksUnsavedChanges 钉住 #50：配置页要有 dirty 追踪，且接上
// 排程页已经建立的两条拦截路径（关/刷新页面、页内切视图）。
func TestConfigPageTracksUnsavedChanges(t *testing.T) {
	src := poolStatsUISource(t)

	// 判据函数必须存在，且与排程页同形（同一套机制，不是第二套）。
	if !strings.Contains(src, "function cfgHasDirty()") {
		t.Fatal("没有 cfgHasDirty()：配置页的改动没有「未保存」判据（issue #50）")
	}
	// 保存按钮要有 dirty 门禁：改没改长得不一样，保存成功后回到禁用态。
	save := strings.Index(src, `$("#btnCfgSave").addEventListener("click", saveConfig)`)
	if save < 0 {
		t.Fatal("找不到保存配置按钮的接线（issue #50）")
	}
	// ① 关/刷新页面时拦。
	unload := poolStatsFuncBodyIndex(src, "beforeunload")
	// 配置页分支必须接在这段监听体里（判据走 cfgLeaveBlocked，与切页路径共用同一个
	// 函数，避免两处各判一次而分叉）。
	if !strings.Contains(unload, "cfgLeaveBlocked()") {
		t.Error("beforeunload 没有配置页分支：改了一半关 tab 静默全丢（issue #50）")
	}
	// 判据本体必须真的看配置页的 dirty，而不是恒 false / 恒 true。
	leave := poolStatsFuncBody(t, src, "cfgLeaveBlocked")
	if !strings.Contains(leave, `VIEW === "config"`) || !strings.Contains(leave, "cfgHasDirty()") {
		t.Error("cfgLeaveBlocked 没有同时看「当前在配置页」与「有未保存改动」（issue #50）")
	}
	if !strings.Contains(unload, "scHasDirty()") {
		t.Error("beforeunload 丢了排程页原有的分支：修 #50 不该把先例拆掉（issue #50）")
	}
	// ② 页内切视图也要拦——用户的症状是「误点导航全部还原」，而 beforeunload
	// 管不到 hash 切页。
	if !strings.Contains(src, "cfgHasDirty()") || strings.Count(src, "cfgHasDirty()") < 3 {
		t.Errorf("cfgHasDirty() 只被引用 %d 次：切页路径没有接上（issue #50）", strings.Count(src, "cfgHasDirty()"))
	}
	// ③ 控件改动要真的点亮 dirty：输入监听必须能到达更新函数。
	if !strings.Contains(src, "function cfgUpdateDirty()") {
		t.Error("没有 cfgUpdateDirty()：控件改动不会更新保存按钮与未保存标记（issue #50）")
	}
}

// panelStyleBlock 取出面板内嵌样式表（不含 JS，免得把 JS 里的字符串当声明数）。
func panelStyleBlock(t *testing.T, src string) string {
	t.Helper()
	open := strings.Index(src, "<style>")
	close := strings.Index(src, "</style>")
	if open < 0 || close < 0 || close < open {
		t.Fatal("index.html has no <style> block")
	}
	return src[open+len("<style>") : close]
}

// poolStatsFuncBodyIndex 取一次 window.addEventListener(evt, …) 之后的片段。
// 判据是「这段监听体里有没有那个分支」，不要求函数的形状。锚点用完整的调用
// 写法（含 "“, e => {"），而不是裸的事件名——事件名也会出现在注释里
// （例如 go() 那段解释为什么要拦 beforeunload 的注释），裸名字会锚错地方。
func poolStatsFuncBodyIndex(src, evt string) string {
	needle := `window.addEventListener("` + evt + `", e => {`
	i := strings.Index(src, needle)
	if i < 0 {
		return ""
	}
	rest := src[i:]
	const span = 700
	if len(rest) > span {
		return rest[:span]
	}
	return rest
}
