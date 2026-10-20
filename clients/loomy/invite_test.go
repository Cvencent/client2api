package loomy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestInviteDefaultIsTheOperatorsCode(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	if got := c.InviteCodeDefault(context.Background()); got != "P276D2" {
		t.Fatalf("default invite code = %q, want P276D2", got)
	}
	overridden := newTestClient(t, `{"invite_code":"OTHER-9"}`, nil)
	if got := overridden.InviteCodeDefault(context.Background()); got != "OTHER-9" {
		t.Fatalf("configured invite code = %q, want OTHER-9", got)
	}
}

func TestBindAccountInviteCodePostsTheDesktopClientsPayload(t *testing.T) {
	rt := &stubTransport{handler: func(r *http.Request, body string) *http.Response {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/points/activation"):
			return jsonResponse(http.StatusOK, okEnvelope(`{"activated":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/points/activation"):
			return jsonResponse(http.StatusOK, okEnvelope(`{"activated":true,"alreadyActivated":false,"inviteeReward":5000,"appliedInvitationCode":"P276D2"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return jsonResponse(http.StatusNotFound, `{}`)
		}
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "acc", testToken, 0)

	res, err := c.BindAccountInviteCode(context.Background(), "acc", "P276D2")
	if err != nil {
		t.Fatalf("BindAccountInviteCode: %v", err)
	}
	if !res.OK || !res.Activated || res.Reward != 5000 || res.AppliedCode != "P276D2" {
		t.Fatalf("result = %+v, want an activated 5000-point binding", res)
	}

	reqs := rt.requests()
	if len(reqs) != 2 {
		t.Fatalf("sent %d requests, want status read + activation", len(reqs))
	}
	post := reqs[1]
	if got := post.Header.Get("token"); got != testToken {
		t.Fatalf("token header = %q, want the session", got)
	}
	var body struct {
		InviteCode string `json:"inviteCode"`
		DeviceID   string `json:"deviceId"`
	}
	if err := json.Unmarshal([]byte(rt.sentBodies()[1]), &body); err != nil {
		t.Fatalf("activation body: %v", err)
	}
	if body.InviteCode != "P276D2" {
		t.Errorf("inviteCode = %q, want P276D2", body.InviteCode)
	}
	if !strings.HasPrefix(body.DeviceID, "loomy-campus-fp-") || len(body.DeviceID) != len("loomy-campus-fp-")+64 {
		t.Errorf("deviceId = %q, want a stable SHA-256 fingerprint", body.DeviceID)
	}
	if got := string(rt.sentBodies()[1]); strings.Contains(got, testToken) {
		t.Error("the session token leaked into the request body")
	}
}

func TestBindAccountInviteCodeDoesNotOverwriteAnotherCode(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{"activated":true,"appliedInvitationCode":"SOMEONE-ELSE"}`))
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "acc", testToken, 0)

	res, err := c.BindAccountInviteCode(context.Background(), "acc", "P276D2")
	if err != nil {
		t.Fatalf("BindAccountInviteCode: %v", err)
	}
	if res.OK || res.Already {
		t.Fatalf("result = %+v, want a refusal for a different existing code", res)
	}
	if !strings.Contains(res.Error, "SOMEONE-ELSE") {
		t.Fatalf("error = %q, want the existing code named", res.Error)
	}
	if rt.count() != 1 {
		t.Fatalf("sent %d requests; an already-activated account must not be rewritten", rt.count())
	}
}

func TestBindAccountInviteCodeIsIdempotentForTheSameCode(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{"activated":true,"appliedInvitationCode":"P276D2"}`))
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "acc", testToken, 0)

	res, err := c.BindAccountInviteCode(context.Background(), "acc", "")
	if err != nil {
		t.Fatalf("BindAccountInviteCode: %v", err)
	}
	if !res.OK || !res.Already || res.AppliedCode != "P276D2" {
		t.Fatalf("result = %+v, want an already-bound success", res)
	}
	if rt.count() != 1 {
		t.Fatalf("sent %d requests, want only the status read", rt.count())
	}
}

func TestRedeemAccountCodeUsesTheRedemptionEndpointNotActivation(t *testing.T) {
	rt := &stubTransport{handler: func(r *http.Request, body string) *http.Response {
		if r.URL.Path != "/api/v1/points/redemption-codes/redeem" {
			t.Fatalf("path = %q, want the redemption endpoint", r.URL.Path)
		}
		return jsonResponse(http.StatusOK, okEnvelope(`{"points":120,"currentBalance":900}`))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "acc", testToken, 0)

	res, err := c.RedeemAccountCode(context.Background(), "acc", " VOUCHER-1 ")
	if err != nil {
		t.Fatalf("RedeemAccountCode: %v", err)
	}
	if !res.OK || res.Points != 120 || res.Balance != 900 {
		t.Fatalf("result = %+v, want 120 points at balance 900", res)
	}
	if got := rt.sentBodies()[0]; got != `{"code":"VOUCHER-1"}` {
		t.Fatalf("body = %s, want the trimmed redemption code", got)
	}
	if got := rt.requests()[0].Header.Get("token"); got != testToken {
		t.Fatalf("token header = %q, want the session", got)
	}
}
