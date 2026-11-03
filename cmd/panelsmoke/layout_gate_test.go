package main

import (
	"os"
	"strings"
	"testing"
)

// 面板门禁要量几何（issue #97）。
//
// 本单的判据：1138 处断言读的是 index.html 的文本、0 处几何——于是「列头被裁
// 掉一行字」「排序箭头被邻列表头盖住」「主按钮白底白字」这类缺陷全部能在测试全绿
// 的情况下上线（#87 那条看守测试在缺陷仍在时就是 PASS 的）。
//
// 真正量几何的断言在 cmd/panelsmoke 的浏览器阶段（assertLayout），它要求真
// 浏览器才能跑；这里钉住的是**那条断言确实在门禁路径上**，以及它用的是几何
// 判据而不是再一次文本匹配。

func TestTheGateMeasuresLayoutNotJustBoot(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(b)

	// 门禁路径必须真的调用几何阶段，且是在同一个浏览器会话里（再开一个浏览器
	// 既浪费一次启动，也可能落到不同的渲染结果上）。
	if !strings.Contains(src, "assertLayout(runCtx, page)") {
		t.Fatal("面板门禁只断言了 data-c2a-ready，没有量任何几何：布局类缺陷仍然无人看守（issue #97）")
	}
	if !strings.Contains(src, `dom, page, closePage, err := smokeBrowser(`) {
		t.Error("几何阶段没有复用 boot 那个浏览器会话")
	}

	body := func(name string) string {
		i := strings.Index(src, "func "+name+"(")
		if i < 0 {
			t.Fatalf("no %s in main.go", name)
		}
		rest := src[i:]
		if j := strings.Index(rest, "\n}\n"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	layout := body("assertLayout")

	// 三条判据必须都在，而且必须是「量出来的」判据。
	for _, want := range []struct{ needle, why string }{
		{"scrollWidth > th.clientWidth", "列头被裁：表头超出列宽时 overflow:hidden 会静默吃掉标签（#87）"},
		{"document.elementFromPoint(", "排序箭头被遮挡：箭头位置的命中测试必须落到自己那一列，否则点击排的是邻列（#87）"},
		{"getComputedStyle", "主按钮对比度：文字与底色同色时标签不可见（#86）"},
		{".box > header details.opmore", "面板块头部的下拉菜单必须真的点得到（#123：菜单渲染正常、每项却命中下面的看板/表格）"},
	} {
		if !strings.Contains(layout, want.needle) {
			t.Errorf("几何断言缺少 %q —— %s", want.needle, want.why)
		}
	}

	// 隐藏的列不算缺陷：余额列在平台不报余额时是**故意**收起的（#44），
	// 把它当失败会让门禁在每个这样的部署上恒红。
	if !strings.Contains(layout, "th0.offsetParent === null") {
		t.Error("几何断言没有跳过被布局收起的列：余额列故意隐藏时会误报（issue #44）")
	}

	// 进账号池必须切到表格呈现：默认是卡片墙，表格在 DOM 里但宽高为 0，
	// 直接量会全部读到 0 并在正确的代码上失败。
	if !strings.Contains(layout, `setAccMode("table")`) {
		t.Error("几何断言没有切到表格呈现：卡片墙下表格是隐藏的，量到的全是 0")
	}

	// 失败信息要能定位：只说「layout regression」而不说哪个元素、差多少，
	// 和不报没有区别。
	if !strings.Contains(layout, "problems.join") || !strings.Contains(layout, "getBoundingClientRect") {
		t.Error("几何断言没有把每个元素的具体数值带进失败信息")
	}

	// The captcha document phase is called from the attempt closure (it needs the
	// base URL), so it is not inside assertLayout's body.  It must still be on the
	// gate path: captcha.html is a second go:embed'ed document with its own inline
	// script, and a break there left every gate green while the overlay hung
	// forever (issue #128).
	if !strings.Contains(src, "assertCaptchaDocs(runCtx, page, base)") {
		t.Error("验证码文档没有进真浏览器阶段：它是独立的 go:embed 文档，坏了五道门禁全绿（issue #128）")
	}
	if !strings.Contains(src, "func assertCaptchaDocs(") {
		t.Error("没有 assertCaptchaDocs 实现")
	}

	// 账号池的键盘行为也要在门禁上（#139/#140）：1138 处 go test 断言全是对
	// index.html 的文本匹配，从来没有真的按过这些键，所以「Enter 被吃掉」和
	// 「焦点被抢回 body」两条都能一路绿灯上线。
	if !strings.Contains(src, "assertAccountsKeyboard(runCtx, page)") {
		t.Error("账号池的键盘检查没有进真浏览器阶段（#139 行内按钮的 Enter 被吞、#140 重建抢走焦点）")
	}
	kb := body("assertAccountsKeyboard")
	for _, want := range []struct{ needle, why string }{
		{`button[data-do]`, "必须以行内动作按钮为判据（#139）"},
		{"defaultPrevented", "必须量 preventDefault：被它拦下就等于按钮不会激活（#139）"},
		{"document.activeElement", "必须量重建后的焦点落点（#140）"},
		{".ac-head", "必须钉住板卡头部仍承担 Enter/空格展开，否则会把 #118 带回来"},
	} {
		if !strings.Contains(kb, want.needle) {
			t.Errorf("键盘检查缺少 %q —— %s", want.needle, want.why)
		}
	}
}
