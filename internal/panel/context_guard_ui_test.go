package panel

import (
	"strings"
	"testing"
)

// TestTrimmedRequestsAreMarkedInTheCallLog 钉住「被网关裁剪过的请求必须显式
// 标出来」这条要求。静默丢掉调用方自己的历史是不允许的，所以调用记录里必须
// 看得见裁了多少、裁前裁后各多大。这些都是 index.html 里的字符串拼接，
// Go 编译器管不到，所以静态钉。
func TestTrimmedRequestsAreMarkedInTheCallLog(t *testing.T) {
	src := poolStatsUISource(t)

	rows := poolStatsFuncBody(t, src, "renderRecentRows")

	// 1. 标记本身。
	if !strings.Contains(rows, "trimTag") {
		t.Error("调用记录行没有接入裁剪标记")
	}
	if !strings.Contains(rows, "x.trimmed") {
		t.Error("裁剪标记没有读 usage 记录上的 trimmed 字段")
	}
	if !strings.Contains(rows, "已裁剪") {
		t.Error("裁剪标记没有中文文案")
	}

	// 2. 悬停要能说清楚裁了多少：条数 + 裁前裁后 token + 窗口。
	for _, want := range []string{
		"x.trim_dropped_messages",
		"x.trimmed_from_tokens",
		"x.trimmed_to_tokens",
		"x.context_window",
		"trimTip",
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("裁剪说明里缺少 %s", want)
		}
	}

	// 3. 标记挂在模型那一列，所以它出现在调用记录里而不是只在数据里。
	if !strings.Contains(rows, `esc(x.model || "-") + trimTag`) {
		t.Error("裁剪标记没有渲染进模型列")
	}
}

// TestLearnedContextSourceHasALabel 钉住「实测来的窗口有自己的人类可读来源」。
// 它必须和「手动」区分开：一个是网关量出来的，一个是人填的。
func TestLearnedContextSourceHasALabel(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, `learned: "实测"`) {
		t.Error("learned 来源没有中文标签")
	}
	// learned 必须不是 manual，否则面板会把网关的测量显示成操作员填的值。
	line := ""
	for _, l := range strings.Split(src, "\n") {
		if strings.Contains(l, "MD_SRC_LABEL = {") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("找不到来源标签映射表")
	}
	if strings.Contains(line, `manual: "实测"`) {
		t.Error("实测来源不能占用 manual 的标签")
	}
}
