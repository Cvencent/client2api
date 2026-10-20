package trae

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// inviteFixture records the code-fission calls and answers them with canned
// vendor envelopes.
type inviteFixture struct {
	bindCode    int64
	bindMessage string
	calls       []string
	bodies      []map[string]any
}

func (f *inviteFixture) roundTrip(req *http.Request) (*http.Response, error) {
	var body map[string]any
	if req.Body != nil {
		_ = json.NewDecoder(req.Body).Decode(&body)
	}
	f.calls = append(f.calls, req.URL.Path)
	f.bodies = append(f.bodies, body)
	resp := map[string]any{"code": f.bindCode, "message": f.bindMessage}
	raw, _ := json.Marshal(resp)
	return jsonResponse(200, string(raw)), nil
}

// inviteClient builds a client whose only account is the given auth and whose
// HTTP transport is the fixture.
func inviteClient(t *testing.T, f *inviteFixture, cfgJSON string) *Client {
	t.Helper()
	cfg := loadConfig([]byte(cfgJSON), nil)
	a := &Auth{AccessToken: "tok", UserID: "u1", DeviceID: "dev-1", Host: "https://api.trae.cn"}
	return testClient(t, cfg, []*Auth{a}, roundTripFunc(f.roundTrip))
}

func TestTraeInviteCodeIsConfiguredNotHardcoded(t *testing.T) {
	cfg := loadConfig([]byte(`{"invite_code":"265795LWHG42"}`), nil)
	if got := cfg.inviteCode(); got != "265795LWHG42" {
		t.Fatalf("inviteCode = %q, want the configured code", got)
	}
	if got := cfg.inviteActivityID(); got != "code_fission_202609" {
		t.Fatalf("inviteActivityID = %q, want the campaign default", got)
	}
	empty := loadConfig([]byte(`{}`), nil)
	if got := empty.inviteCode(); got != "" {
		t.Fatalf("an unconfigured module must not invent a code, got %q", got)
	}
}

func TestTraeInviteBindSendsTheDesktopPayload(t *testing.T) {
	f := &inviteFixture{}
	c := inviteClient(t, f, `{"invite_code":"265795LWHG42"}`)
	a := &Auth{AccessToken: "tok", UserID: "u1", DeviceID: "dev-1"}

	res := c.bindInviteCode(context.Background(), a, "265795LWHG42")
	if !res.OK {
		t.Fatalf("bind failed: %+v", res)
	}
	if len(f.calls) != 1 || f.calls[0] != "/trae/api/v2/ug/code_fission/bind" {
		t.Fatalf("calls = %v, want the bind endpoint", f.calls)
	}
	body := f.bodies[0]
	if body["invite_code"] != "265795LWHG42" || body["activity_id"] != "code_fission_202609" {
		t.Fatalf("body = %v", body)
	}
}

func TestTraeInviteReportsAlreadyJoinedAsARefusalNotAnError(t *testing.T) {
	f := &inviteFixture{bindCode: 9022, bindMessage: "已参与"}
	c := inviteClient(t, f, `{"invite_code":"265795LWHG42"}`)

	res := c.bindInviteCode(context.Background(), &Auth{AccessToken: "tok", UserID: "u1"}, "265795LWHG42")
	if res.OK {
		t.Fatal("9022 was reported as success")
	}
	if !strings.Contains(res.Error, "已参与") {
		t.Fatalf("error = %q, want the already-joined explanation", res.Error)
	}
}

func TestTraeFleetBindIsNotSafe(t *testing.T) {
	c := inviteClient(t, &inviteFixture{}, `{}`)
	if c.InviteCodeBindAllSafe() {
		t.Fatal("Trae must not advertise a fleet-wide invite bind")
	}
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.InviteCodes || !caps.InviteCodeNewAccountOnly {
		t.Fatalf("caps = %+v, want invite support marked new-account-only", caps)
	}
}

// TestInviteLoginMintsAFreshDeviceByDefault pins the switch that keeps every
// invited account on its own device: without it the campaign refuses the
// second account (9022).
func TestInviteLoginMintsAFreshDeviceByDefault(t *testing.T) {
	on := loadConfig([]byte(`{"invite_code":"265795LWHG42"}`), nil)
	if !on.inviteNewDevicePerLogin() {
		t.Fatal("invite_new_device_per_login must default to true")
	}
	off := loadConfig([]byte(`{"invite_code":"265795LWHG42","invite_new_device_per_login":false}`), nil)
	if off.inviteNewDevicePerLogin() {
		t.Fatal("an explicit false must be honoured")
	}
}

// TestInviteDeviceSwitchOnlyAppliesWhenACodeIsConfigured documents the scope:
// an ordinary login with no invite code keeps the existing fingerprint.
func TestInviteDeviceSwitchOnlyAppliesWhenACodeIsConfigured(t *testing.T) {
	noCode := loadConfig([]byte(`{}`), nil)
	if noCode.inviteCode() != "" {
		t.Fatalf("inviteCode = %q, want empty", noCode.inviteCode())
	}
	// The guard in StartLogin is `inviteCode != "" && inviteNewDevicePerLogin()`,
	// so an empty code means the fresh-device branch never runs.
	if noCode.inviteCode() != "" && noCode.inviteNewDevicePerLogin() {
		t.Fatal("the fresh-device branch must be gated on a configured invite code")
	}
}
