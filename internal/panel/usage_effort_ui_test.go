package panel

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"client2api/internal/gateway"
)

// 「调用记录」每行要显示这次请求带过来的思考强度。网关把它记在
// UsageRecord.ReasoningEffort 里，所以这里同时钉住两件事：JSON 键名是网关真的
// 会写出去的那个，以及表头、行模板、空表 colspan 三处一起长了一列。
func TestRecentCallsShowTheReasoningEffort(t *testing.T) {
	src := poolStatsUISource(t)

	// 界面读的键必须逐字等于网关写进 JSON 的 tag，否则这一列永远是空的。
	b, err := json.Marshal(gateway.UsageRecord{ReasoningEffort: "xhigh"})
	if err != nil {
		t.Fatalf("marshal UsageRecord: %v", err)
	}
	if !strings.Contains(string(b), `"reasoning_effort":"xhigh"`) {
		t.Fatalf("UsageRecord 的 JSON 里没有 reasoning_effort: %s", b)
	}
	if !strings.Contains(src, "x.reasoning_effort") {
		t.Error("renderRecentRows 没有读 x.reasoning_effort：这一列取不到值")
	}
	if !strings.Contains(src, "思考强度") {
		t.Error("「调用记录」表头没有「思考强度」列")
	}

	// 表头与行模板必须一样宽：只改一边，整张表就错位。
	if h, r := recentHeaderCells(t, src), recentRowCells(t, src); h != r {
		t.Errorf("最近调用表头 %d 列、行模板 %d 格：多一列不改另一处会整行错位", h, r)
	}
	if h := recentHeaderCells(t, src); h <= 12 {
		t.Errorf("最近调用只有 %d 列，思考强度这一列没加上", h)
	}
	if body := poolStatsFuncBody(t, src, "renderRecentRows"); !strings.Contains(body, `colspan="16"`) {
		t.Error("空表的 colspan 没有从 13 跟到 16")
	}
}

// recentHeaderCells 数出「最近调用」那张表的表头列数。
func recentHeaderCells(t *testing.T, src string) int {
	t.Helper()
	start := strings.Index(src, `id="usage-panel-recent"`)
	if start < 0 {
		t.Fatal("找不到最近调用的面板")
	}
	end := strings.Index(src[start:], `id="usage-panel-stats"`)
	if end < 0 {
		t.Fatal("找不到统计面板，最近调用这一段没有边界")
	}
	thead := regexp.MustCompile(`(?s)<thead>.*?</thead>`).FindString(src[start : start+end])
	if thead == "" {
		t.Fatal("最近调用面板里找不到 <thead>")
	}
	// <thead> 本身含有 "<th"，所以只数真正的表头标签。
	return len(regexp.MustCompile(`<th[ >]`).FindAllString(thead, -1))
}

// recentRowCells 数出 renderRecentRows 行模板里的 <td 个数。
func recentRowCells(t *testing.T, src string) int {
	t.Helper()
	body := poolStatsFuncBody(t, src, "renderRecentRows")
	start := strings.Index(body, "return '<tr>")
	if start < 0 {
		t.Fatal("renderRecentRows 里找不到行模板 return '<tr>")
	}
	rest := body[start:]
	end := strings.Index(rest, "';")
	if end < 0 {
		t.Fatal("renderRecentRows 的行模板看起来没结束")
	}
	return strings.Count(rest[:end], "<td")
}
