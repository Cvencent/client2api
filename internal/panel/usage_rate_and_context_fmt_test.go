package panel

import (
	"strings"
	"testing"
)

// 钉住用量页零样本与上下文显示两处修复（issue #185 / #187）。

// #185：一条请求都没有时，成功率磁贴必须是「—」加「还没有请求」，不许把
// 0/0 兜底成 100% 满格绿条——「0 次失败」和「0 次尝试」是两回事。
func TestUsageSuccessRateShowsADashWithoutSamples(t *testing.T) {
	src := poolStatsUISource(t)
	usage := poolStatsFuncBody(t, src, "renderUsage")
	if !strings.Contains(usage, "const rate = req ? Math.round((req - fail) / req * 100) : null") {
		t.Fatal("renderUsage 仍在把零样本兜底成 100：一条请求都没有会被渲染成最好的成绩（issue #185）")
	}
	tile := poolStatsFuncBody(t, src, "renderUsageTiles")
	if !strings.Contains(tile, `v: rate == null ? "—" : rate + "%"`) {
		t.Fatal("成功率磁贴没有空值档：零样本要显示「—」（与总览页对齐，issue #185）")
	}
	if !strings.Contains(tile, "还没有请求") {
		t.Error("零样本的副标题应说「还没有请求」；「没有失败」只属于真跑过流量的场景（issue #185）")
	}
}

// #187：上下文窗口是容量值，不许走 fmtK 的有损舍入（1,048,576 → 1049K，
// 多算 424）；三个消费点（表格列、卡片摘要、3D 检视行）一律 numf 全量写出。
func TestContextLengthNeverRoundsToK(t *testing.T) {
	src := poolStatsUISource(t)

	// 表格列（renderModels 的 tbody 拼接里）
	tables := poolStatsFuncBody(t, src, "renderModels")
	if !strings.Contains(tables, "esc(numf(ctx))") {
		t.Fatal("模型表「上下文」列仍用 fmtK：1,048,576 会显示成 1049K，多算 424（issue #187）")
	}
	// 卡片摘要「最大上下文」
	if strings.Contains(src, `["最大上下文", maxCtx ? fmtK(maxCtx) : "—"]`) {
		t.Error("模型卡摘要的「最大上下文」仍用 fmtK（issue #187）")
	}
	// 3D 检视行
	if strings.Contains(src, `fmtK(m.context_length)`) || strings.Contains(src, `fmtK(m.max_output_tokens)`) {
		t.Error("3D 检视的容量行仍用 fmtK（issue #187）")
	}
	// fmtK 的定义注释必须声明它的边界：速率类专用，容量类禁用（注释在函数
	// 上方，函数体提取器拿不到，直接看定义前的一段源码）。
	defAt := strings.Index(src, "function fmtK(")
	if defAt < 0 || !strings.Contains(src[max(0, defAt-400):defAt], "容量一律走 numf") {
		t.Error("fmtK 缺少「速率类专用」的边界注释：下一个容量消费点还会把它捡回去（issue #187）")
	}
}
