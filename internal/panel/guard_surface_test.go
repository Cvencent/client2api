package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// WAF IP 闸与降级提示词窗口的面板出口（issue #98）。

// guardCore builds a live Guard and trips the requested gate exactly the way
// the gateway does.
func tripGuard(t *testing.T, gate string) *core.Guard {
	t.Helper()
	g := core.NewGuard(func(string, ...any) {})
	switch gate {
	case "waf":
		for i := 0; i < core.WAFIPThreshold; i++ {
			g.ReportWAF(string(rune('a' + i))) // distinct accounts
		}
	case "degrade":
		g.ReportContentBlock()
	}
	return g
}

// TestOverviewCarriesTheGuardWhenAGateIsOpen pins the payload half: the
// overview carries the guard snapshot when a gate is open and omits it
// entirely when both are closed (the zero value is the common case and the
// page reads its absence as "nothing to announce").
func TestOverviewCarriesTheGuardWhenAGateIsOpen(t *testing.T) {
	for _, tc := range []struct {
		gate     string
		wantWAF  bool
		wantDegr bool
	}{
		{"waf", true, false},
		{"degrade", false, true},
	} {
		g := tripGuard(t, tc.gate)
		p := New(Options{Version: "test", Guard: g})
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: overview = %d (%s)", tc.gate, rec.Code, rec.Body.String())
		}
		var out struct {
			Guard *core.GuardSnapshot `json:"guard"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: decode: %v", tc.gate, err)
		}
		if out.Guard == nil {
			t.Fatalf("%s: overview carries no guard while the gate is active (issue #98)", tc.gate)
		}
		if out.Guard.WAFActive != tc.wantWAF || out.Guard.DegradeActive != tc.wantDegr {
			t.Fatalf("%s: guard = %+v, want waf=%v degrade=%v", tc.gate, *out.Guard, tc.wantWAF, tc.wantDegr)
		}
		if tc.wantWAF && out.Guard.WAFUntil == "" {
			t.Fatalf("%s: waf_active with no until; the banner cannot say 恢复时刻 (issue #98)", tc.gate)
		}
	}
}

func TestOverviewOmitsTheGuardWhenBothGatesAreClosed(t *testing.T) {
	p := New(Options{Version: "test", Guard: core.NewGuard(func(string, ...any) {})})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	var out struct {
		Guard *core.GuardSnapshot `json:"guard"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Guard != nil {
		t.Fatalf("guard = %+v, want absent when both gates are closed (issue #98)", *out.Guard)
	}
}

// The UI half: the overview renderer announces an open gate in the pills row.
func TestOverviewBannerAnnouncesAnOpenGate(t *testing.T) {
	src := poolStatsUISource(t)
	ov := poolStatsFuncBody(t, src, "renderOverview")
	for _, want := range []string{
		`WAF 拦截：出口 IP 轮转已暂停`,
		`降级提示词生效中`,
		`guardInfo.waf_active`,
		`guardInfo.degrade_active`,
	} {
		if !strings.Contains(ov, want) {
			t.Errorf("renderOverview 的全局状态条缺少 %q：WAF 闸/降级窗口开着时面板无法解释整站异常（issue #98）", want)
		}
	}
}
