package raccoon

import (
	"context"
	"testing"

	"client2api/internal/core"
)

// The panel renders the 接码 and 自动登录 controls from these capability flags.
// A module can implement the interfaces and still be hidden if the values do
// not line up, so this pins the whole handshake rather than the interfaces in
// isolation.
func TestRaccoonReportsSMSAndAutoLoginCapabilities(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{}`, nil)
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.SMS {
		t.Error("capabilities did not report sms")
	}
	if !caps.AutoLogin {
		t.Error("capabilities did not report auto_login")
	}
}
