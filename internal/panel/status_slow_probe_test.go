package panel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"client2api/internal/gateway"
)

// /panel/api/status 的 p99 观测（issue #96）。
//
// 现象：偶发 ~5s 尖刺、瞬时恢复，怀疑与 5 分钟余额扫描的锁面相关——归因需要
// 一周量级的样本。观测本身就是本单的产出：>1s 的响应写进运行日志一行，带耗时
// 与落入 5 分钟 tick 窗口的位置；跑一周即可看尖刺与 sweep tick 的相关性，
// 再决定是否缩小锁面或改原子快照。

func TestStatusSlowResponseIsLoggedWithTheTickOffset(t *testing.T) {
	// The probe keys on elapsed time, which a fast handler never crosses --
	// the wiring, not a slowdown, is what the week of samples depends on.
	b, err := os.ReadFile("panel.go")
	if err != nil {
		t.Skipf("cannot read panel.go: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		"slow /panel/api/status",
		"into 5m tick window",
		"d > time.Second",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("handleStatus 的慢响应探针缺少 %q（issue #96 的观测线）", want)
		}
	}

	// 行为半区：哪怕手头 handler 很快，日志环写入的代码路径是真实的——
	// 手工塞一行验证环与格式，一周后在运行日志里能按这一行筛出全部尖刺。
	ring := gateway.NewLogRing(200)
	// New returns the http.Handler; the concrete panel keeps opts, which is
	// where the log ring lives (handleStatus reads p.opts.Logs on the slow path).
	pp := &panel{opts: Options{Version: "test", Logs: ring}}
	pp.wireStoreLogs()
	pp.opts.Logs.Add("panel: slow /panel/api/status: 5s0ms (into 5m tick window: 2m30s)")
	h := New(Options{Version: "test", Logs: ring})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs?lines=500", nil))
	if !strings.Contains(rec.Body.String(), "slow /panel/api/status") {
		t.Fatal("慢响应行没有落进运行日志：观测线在真实面板上不可见（issue #96）")
	}
	if !strings.Contains(rec.Body.String(), "into 5m tick window") {
		t.Fatal("慢响应行缺 tick 窗口偏移：无法与 sweep tick 做相关性（issue #96）")
	}
}
