package panel

import (
	"strings"
	"testing"
)

// TestAccountRiskAndCredentialFaultRenderAsSeparateRedStates pins the two
// operator actions the panel must keep apart.  Platform risk control cannot be
// fixed by logging in again; a dead token or session can.
func TestAccountRiskAndCredentialFaultRenderAsSeparateRedStates(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "accountState")

	for _, want := range []string{
		`const note = String((a && a.note) || "").toLowerCase();`,
		`raw === "risk" || raw === "fault" || raw === "account_fault" || note.startsWith("account_fault")`,
		`return { key: "risk", label: "风控", cls: "err" };`,
		`raw === "invalid" || raw === "expired" || raw === "unauthorized"`,
		`return { key: "fault", label: "账号异常", cls: "err" };`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("accountState 没有把风控和账号异常分开，缺：%s", want)
		}
	}
}

// TestRiskAndAccountFaultHaveSeparateCounters pins the dashboard split.  A
// single red bucket would hide whether the operator should wait/appeal or use
// the re-login action.
func TestRiskAndAccountFaultHaveSeparateCounters(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		`id="sRisk"`,
		`id="sFault"`,
		`风控`,
		`账号异常`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("统计区域缺少独立计数入口：%s", want)
		}
	}

	body := poolStatsFuncBody(t, src, "renderStats")
	for _, want := range []string{
		`let total = 0, ready = 0, cool = 0, risk = 0, fault = 0, disabled = 0;`,
		`else if (cls.key === "risk") risk++;`,
		`else if (cls.key === "fault") fault++;`,
		`else if (cls.key === "disabled") disabled++;`,
		`$("#sRisk").textContent = risk;`,
		`$("#sFault").textContent = fault;`,
		`$("#sDisabled").textContent = disabled;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderStats 没有分别统计风控和账号异常：%s", want)
		}
	}

	for _, want := range []string{
		`risk: "风控"`,
		`account_fault: "风控"`,
		`fault: "账号异常"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("状态说明没有统一成新的中文词：%s", want)
		}
	}
}
