package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/gateway"
)

// 日志页两段式轮询的服务端契约（issue #92）。
//
// meta=1 的轻查询只回 total/capacity——前端 4 秒一拍先问这个（约百字节），
// total 没变就不取 500 行正文（约 133 KB）。这个 handler 测试钉住三件事：
// 轻查询不带 entries、不带 meta 的完整查询照旧、未知参数不破坏原语义。

func TestLogsMetaProbeCarriesOnlyTheCounters(t *testing.T) {
	ring := gateway.NewLogRing(200)
	ring.Add("line one")
	ring.Add("line two")

	p := New(Options{Version: "test", Logs: ring})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs?meta=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("meta probe = %d (%s)", rec.Code, rec.Body.String())
	}
	var meta struct {
		Entries  []json.RawMessage `json:"entries"`
		Total    int64             `json:"total"`
		Capacity int               `json:"capacity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("meta probe is not the counter envelope: %v: %s", err, rec.Body.String())
	}
	if meta.Entries != nil {
		t.Fatalf("meta probe carried %d entries; it must be counters only or the 4s poll gains nothing (issue #92)", len(meta.Entries))
	}
	if meta.Total != 2 {
		t.Fatalf("meta total = %d, want 2", meta.Total)
	}
	if meta.Capacity != 200 {
		t.Fatalf("meta capacity = %d, want 200", meta.Capacity)
	}
}

func TestLogsFullQuerySurvivesTheMetaProbe(t *testing.T) {
	ring := gateway.NewLogRing(200)
	ring.Add("line one")
	ring.Add("line two")

	p := New(Options{Version: "test", Logs: ring})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs?lines=500", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("full query = %d (%s)", rec.Code, rec.Body.String())
	}
	var full struct {
		Entries []gateway.LogEntry `json:"entries"`
		Total   int64              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &full); err != nil {
		t.Fatalf("full query is not the entries envelope: %v", err)
	}
	if len(full.Entries) != 2 || full.Total != 2 {
		t.Fatalf("full query = %d entries / total %d, want 2 / 2 (the meta probe must not change the body contract)", len(full.Entries), full.Total)
	}
}

// 前端半区（issue #92）：renderLogs 先问 meta、total 没变就早退——两段式
// 轮询的 UI 侧契约。
//
// 这一条曾经把「meta 戳带 `"meta:"` 前缀」写成断言（当时的理由是「三种状态不能
// 互相去重」，#84 的教训）。那个形状本身是缺陷：正文写完后把 LOGSEEN 覆盖成
// `"ok:"` 前缀，下一拍的 meta 戳永远对不上，早退分支一次都命不中——测试全绿而
// 两段式从未生效（issue #113）。三种状态本来就由 total 与 LOGCH 区分，错误态另有
// `"err"` 这个值，不需要靠前缀。所以这里改成钉「meta 先问、同形戳早退、正文按需
// 拉取」这三件行为，具体戳形态由 log_poll_incremental_ui_test.go 钉。
func TestLogsRenderUsesTheTwoPhasePoll(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderLogs")

	if !strings.Contains(body, `api("/panel/api/logs?meta=1")`) {
		t.Fatal("renderLogs 没有先问 meta 轻查询：静止时每 4 秒整份重下 500 行（issue #92）")
	}
	// 早退必须基于 meta 读回来的 total，且判据与正文同形（issue #113）。
	if !strings.Contains(body, `const stamp = "ok:" + String(metaTotal) + "|" + LOGCH;`) {
		t.Fatal("meta 轮询没有按 total 早退：轻查询等于白做（issue #92）")
	}
	if !strings.Contains(body, "if (stamp === LOGSEEN) return;") {
		t.Fatal("meta 轮询的早退判据没接上：轻查询等于白做（issue #92）")
	}
	// 正文路径仍在，且 total 变了走增量而不是整份重下。
	if !strings.Contains(body, `api("/panel/api/logs?lines=500")`) {
		t.Fatal("renderLogs 丢了整份正文拉取：首读 / 换频道时页面不更新（issue #92）")
	}
	if !strings.Contains(body, `"/panel/api/logs?since="`) {
		t.Fatal("renderLogs 没有增量拉取：total 一变就整份重下 500 行（issue #113）")
	}
}
