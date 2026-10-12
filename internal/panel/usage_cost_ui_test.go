package panel

import (
	"strings"
	"testing"
)

func TestUsageOverviewShowsCacheHitRateAndTotalCost(t *testing.T) {
	src := poolStatsUISource(t)
	tiles := poolStatsFuncBody(t, src, "renderUsageTiles")
	for _, want := range []string{
		`k: "缓存命中率"`,
		`k: "总花费"`,
		"t.cache_hit_tokens",
		"t.cache_prompt_tokens",
		"usageCacheHitRate(cacheHit, cachePrompt, t.has_cached_tokens)",
		"t.has_cost",
		"t.unpriced_requests",
		"未定价",
		"—",
	} {
		if !strings.Contains(tiles, want) {
			t.Errorf("用量总览没有显示 %s", want)
		}
	}
}

func TestRecentCallsShowCacheAndCost(t *testing.T) {
	src := poolStatsUISource(t)
	if cost := poolStatsFuncBody(t, src, "fmtUsageCost"); !strings.Contains(cost, "¥") {
		t.Error("花费格式器没有使用人民币符号")
	}
	for _, want := range []string{"缓存 tokens", "缓存命中率", "花费", "¥/百万 tokens"} {
		if !strings.Contains(src, want) {
			t.Errorf("最近调用缺少 %q", want)
		}
	}
	body := poolStatsFuncBody(t, src, "renderRecentRows")
	for _, want := range []string{
		"x.cached_tokens",
		"x.has_cached_tokens",
		"x.prompt_tokens",
		"x.cost",
		"x.has_cost",
		"—",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderRecentRows 没有读取 %s", want)
		}
	}
	if h, r := recentHeaderCells(t, src), recentRowCells(t, src); h != r {
		t.Errorf("最近调用表头 %d 列、行模板 %d 格：新增列后整行会错位", h, r)
	}
	if !strings.Contains(body, `colspan="17"`) {
		t.Error("最近调用空表的 colspan 没有跟着新增列更新到 17")
	}
}

func TestUsageCacheHitRateAlwaysKeepsOneDecimal(t *testing.T) {
	src := poolStatsUISource(t)
	format := poolStatsFuncBody(t, src, "fmtUsageCacheHitRate")
	if !strings.Contains(format, ".toFixed(1)") {
		t.Error("缓存命中率没有固定保留一位小数：99.6% 会被四舍五入成 100%")
	}

	tiles := poolStatsFuncBody(t, src, "renderUsageTiles")
	if !strings.Contains(tiles, "fmtUsageCacheHitRate(hit)") {
		t.Error("总览磁贴没有走统一的一位小数格式化")
	}
	recent := poolStatsFuncBody(t, src, "renderRecentRows")
	if !strings.Contains(recent, "fmtUsageCacheHitRate(cacheHit)") {
		t.Error("最近调用没有走统一的一位小数格式化")
	}
}
