package panel

import (
	"strings"
	"testing"
)

// TestAccountFaultRendersAsRedState pins the account-fault split. WorkBuddy
// uses exhausted for both credit exhaustion and account faults, but only the
// former is a cooldown. A fault must stay visibly actionable in red even when
// a persisted record was written by an older build as exhausted + account_fault.
func TestAccountFaultRendersAsRedState(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "accountState")
	for _, want := range []string{
		`const note = String((a && a.note) || "").toLowerCase();`,
		`raw === "fault" || raw === "account_fault" || note.startsWith("account_fault")`,
		`return { key: "disabled", label: "账号异常", cls: "err" };`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("accountState 没有把账号异常渲染成红色中文状态，缺：%s", want)
		}
	}
}
