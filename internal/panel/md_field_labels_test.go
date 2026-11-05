package panel

import (
	"strings"
	"testing"
)

// 钉住模型详情区表单控件的可编程名称（issue #213，WCAG 1.3.1/4.1.2）：
//
//	10 个可编辑字段的视觉标签只画在 td[data-field]::before 里——AX 里 40 个
//	控件空名（两个语义相反的数字框都是「spin button」，三个价格框共用同一句
//	长提示彼此不可分）。aria-label 与 ::before 同源（MD_FIELD_LABELS）。
func TestModelDetailInputsCarryNames(t *testing.T) {
	src := poolStatsUISource(t)

	// 名字表与 CSS ::before 同源定义（一处定义两处复用）。
	// 且必须在脚本区（issue #216）：曾整块被写进 <style>，CSS 解析器吞掉后
	// JS 无绑定，模型页整页 ReferenceError。
	if !strings.Contains(src, "const MD_FIELD_LABELS = {") {
		t.Fatal("MD_FIELD_LABELS 不见了：aria-label 与视觉标签会各自漂移（issue #213）")
	}
	styleEnd := strings.Index(src, "</style>")
	declAt := strings.Index(src, "const MD_FIELD_LABELS = {")
	if styleEnd < 0 || declAt < styleEnd {
		t.Fatalf("MD_FIELD_LABELS 不在脚本区（</style> @%d, decl @%d）：会被 CSS 解析器吞掉（issue #216）", styleEnd, declAt)
	}
	// 五个生成函数的 output 都带 aria-label。
	for _, fn := range []string{"mdNumCell", "mdPriceCell", "mdDefaultEffortCell", "mdEffortsCell"} {
		body := poolStatsFuncBody(t, src, fn)
		if !strings.Contains(body, "aria-label=") {
			t.Errorf("%s 生成的 input 没有 aria-label：读屏拿不到字段名（issue #213）", fn)
		}
	}
	// 三个价格框的名字互不相同：price-input/price-output/price-cache 各自入表。
	for _, key := range []string{"price-input", "price-output", "price-cache"} {
		if !strings.Contains(src, `"`+key+`": "`) && !strings.Contains(src, `"`+key+`":"`) {
			t.Errorf("价格键 %q 不在 MD_FIELD_LABELS：三格共用同一句提示不可分（issue #213）", key)
		}
	}
	// mappings 输入框也要有名字（它原来只有 title）。
	maps := poolStatsFuncBody(t, src, "mdElevateTopCell")
	if !strings.Contains(maps, `aria-label="思考档位映射"`) {
		t.Error("思考档位映射输入框没有 aria-label（issue #213）")
	}
	// effort_mappings 的输入带 aria-label（在 mdElevateTopCell 内）。
	if !strings.Contains(maps, "aria-label=\"思考档位映射\"") {
		t.Error("思考档位映射输入框没有 aria-label：读屏不知道这条规则是给谁配的（issue #213）")
	}
}
