package panel

import (
	"strings"
	"testing"
)

// TestStickySessionTileOpensADialog pins the discoverability contract: the
// homepage statistic is a real button, and the dialog it opens owns the row
// actions the operator asked for.
func TestStickySessionTileOpensADialog(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`id="sStickyTile"`,
		`id="stickyVeil"`,
		`id="stickyRows"`,
		`id="btnStickyClose"`,
		`data-sticky-action="unbind"`,
		`data-sticky-action="switch"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("粘性会话界面缺少 %s", want)
		}
	}
	if !strings.Contains(src, `$("#sStickyTile").addEventListener("click"`) {
		t.Error("粘性会话数字卡没有点击入口")
	}
}

// TestStickySessionDialogUsesTheManagementRoutes keeps the UI wired to the
// aggregate API rather than reimplementing routing policy in the browser.
func TestStickySessionDialogUsesTheManagementRoutes(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`api("/panel/api/sticky-sessions"`,
		`/panel/api/sticky-sessions/unbind`,
		`/panel/api/sticky-sessions/switch`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("粘性会话界面没有使用管理接口 %s", want)
		}
	}
}
