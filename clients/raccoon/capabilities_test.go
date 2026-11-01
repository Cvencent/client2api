package raccoon

import (
	"context"
	"testing"

	"client2api/internal/core"
)

// The panel renders the 接码 and 自动登录 controls from these capability flags:
// a flag is a plain interface assertion, so dropping the methods is what hides
// the button.  This pins the whole handshake rather than the interfaces in
// isolation, because a stray method would silently take the button away.
//
// The auto-login flag itself is deliberate: the browser drives the loopback
// page (autologin.go), so the Aliyun traceless captcha runs inside a real
// Chromium exactly as it does for a manual sign-in.  The 2026-10-21 removal
// only ever killed the bypass-the-browser path.
func TestRaccoonReportsSMSAndAutoLoginCapabilities(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{}`, nil)
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.SMS {
		t.Error("capabilities did not report sms; the manual 接码 controls would vanish")
	}
	if !caps.AutoLogin {
		t.Error("capabilities did not report auto_login; the 一键添加 button would vanish")
	}
	if _, ok := any(c).(core.AutoLoginProvider); !ok {
		t.Error("the module no longer implements core.AutoLoginProvider")
	}
}
