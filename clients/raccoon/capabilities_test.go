package raccoon

import (
	"context"
	"testing"

	"client2api/internal/core"
)

// The panel renders the 接码 and 自动登录 controls from these capability flags:
// a flag is a plain interface assertion, so dropping the methods is what hides
// the button.  This pins the whole handshake rather than the interfaces in
// isolation, because a stray method would silently bring the button back.
func TestRaccoonReportsSMSButNotAutoLoginCapabilities(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{}`, nil)
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.SMS {
		t.Error("capabilities did not report sms; the manual 接码 controls would vanish")
	}
	if caps.AutoLogin {
		t.Error("capabilities reported auto_login: the 一键接码 button would come back")
	}
	if _, ok := any(c).(core.AutoLoginProvider); ok {
		t.Error("the module still implements core.AutoLoginProvider")
	}
}
