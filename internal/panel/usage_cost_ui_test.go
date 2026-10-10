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
	if !strings.Contains(body, `colspan="16"`) {
		t.Error("最近调用空表的 colspan 没有跟着新增三列从 13 更新到 16")
	}
}
