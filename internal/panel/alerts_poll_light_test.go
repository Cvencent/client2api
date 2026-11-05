package panel

import (
	"os"
	"strings"
	"testing"
)

// 钉住告警轮询的三层契约（issue #208）：
//
//	5 秒一拍的 refreshAlertBadge 曾打全量 GET——200 条上限时 49.8KB/拍
//	（一拍的八成），只为读 unread 一个整数。三层修法：徽标走 count=1 轻查询；
//	全量 GET 补强 ETag（只对 alerts[] 求哈希，不算 unread）；MarkAllRead
//	水位早退（未读已 0 不再整份重写 63KB）。
func TestAlertBadgeUsesTheLightQuery(t *testing.T) {
	src := poolStatsUISource(t)
	badge := poolStatsFuncBody(t, src, "refreshAlertBadge")
	if !strings.Contains(badge, `"/panel/api/alerts?count=1"`) {
		t.Fatal("refreshAlertBadge 仍打全量 GET：200 条告警时每拍 49.8KB 只为读一个整数（issue #208）")
	}
	// 告警视图下的重复收敛：refreshAll 的 badge 调用在 alerts 视图跳过。
	refresh := poolStatsFuncBody(t, src, "refreshAll")
	if !strings.Contains(refresh, `if (VIEW !== "alerts") refreshAlertBadge()`) {
		t.Error("refreshAll 没有在告警视图跳过 badge 调用：同一拍 4 次全量 GET（issue #208）")
	}
}

func TestAlertsGetServesETagAndCount(t *testing.T) {
	// count=1 分支与 ETag 在服务端 alerts.go（不在 index.html），直接读它。
	raw, err := os.ReadFile("alerts.go")
	if err != nil {
		t.Fatalf("read alerts.go: %v", err)
	}
	src := string(raw)
	// count=1 分支存在且只回 unread。
	if !strings.Contains(src, `r.URL.Query().Get("count") == "1"`) {
		t.Fatal("alerts GET 没有 count=1 轻查询分支（issue #208）")
	}
	// ETag 只对 alerts[] 求哈希：unread 算进去会让 304 在标记已读后失效。
	etag := strings.Index(src, `sum := sha256.Sum256(body)`)
	if etag < 0 {
		t.Fatal("alerts GET 没有 ETag")
	}
	// sha256 之前的 marshal 目标必须是 out.Alerts（而非包含 unread 的 out）。
	segFrom := etag - 200
	if segFrom < 0 {
		segFrom = 0
	}
	seg := src[segFrom:etag]
	if !strings.Contains(seg, "Marshal(out.Alerts)") {
		t.Errorf("ETag 哈希的不是 alerts[]：标记已读会改变哈希，304 永远失效（issue #208）segment=%s", seg)
	}
	if !strings.Contains(src, `w.WriteHeader(http.StatusNotModified)`) {
		t.Error("alerts GET 没有 304 分支")
	}
}
