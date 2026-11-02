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
// 轮询的 UI 侧契约。meta 戳带 "meta:" 前缀，与 "ok:" 正文戳 / "err" 错误戳
// 互不同名（issue #84 的教训：三种状态不能互相去重）。
func TestLogsRenderUsesTheTwoPhasePoll(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderLogs")

	if !strings.Contains(body, `api("/panel/api/logs?meta=1")`) {
		t.Fatal("renderLogs 没有先问 meta 轻查询：静止时每 4 秒整份重下 500 行（issue #92）")
	}
	if !strings.Contains(body, `const stamp0 = "meta:" + String((meta.data || {}).total || 0) + "|" + LOGCH;`) {
		t.Fatal("meta 轮询没有独立命名空间的去重戳：会和正文/错误戳互相吞（issue #92）")
	}
	if !strings.Contains(body, `if (stamp0 === LOGSEEN) return;`) {
		t.Fatal("meta 轮询没有按 total 早退：轻查询等于白做（issue #92）")
	}
	// 正文路径仍在：total 变了必须把 500 行拉回来。
	if !strings.Contains(body, `api("/panel/api/logs?lines=500")`) {
		t.Fatal("renderLogs 丢了正文拉取：total 变化后页面不更新（issue #92）")
	}
}
