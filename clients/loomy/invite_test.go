package loomy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestInviteDefaultIsTheOperatorsCode(t *testing.T) {
	// With an empty pool the picker has nothing to read, so the configured
	// default still answers.
	c := newTestClient(t, "{}", nil)
	if got := c.InviteCodeDefault(context.Background()); got != "P276D2" {
		t.Fatalf("default invite code = %q, want P276D2", got)
	}
	overridden := newTestClient(t, `{"invite_code":"OTHER-9"}`, nil)
	if got := overridden.InviteCodeDefault(context.Background()); got != "OTHER-9" {
		t.Fatalf("configured invite code = %q, want OTHER-9", got)
	}
}

// inviteCodesStub answers /invitation-codes per token and /points/activation
// as "not activated", so a seeded pool with distinct codes can exercise the
// picker.  The map keys are tokens, not account ids, because that is what the
// vendor authenticates with.
type inviteCodesStub struct {
	mu    sync.Mutex
	codes map[string][]inviteCodeUse
	// hits counts /invitation-codes GETs, so tests can assert the memo.
	hits int
}

func (s *inviteCodesStub) handler(r *http.Request, body string) *http.Response {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/invitation-codes"):
		s.mu.Lock()
		s.hits++
		codes, ok := s.codes[r.Header.Get("token")]
		s.mu.Unlock()
		if !ok {
			return jsonResponse(http.StatusOK, okEnvelope(`{"list":[]}`))
		}
		rows := make([]string, 0, len(codes))
		for _, c := range codes {
			rows = append(rows, fmt.Sprintf(`{"inviteCode":%q,"usedCount":%d,"maxUses":%d,"status":%q}`,
				c.InviteCode, c.UsedCount, c.MaxUses, c.Status))
		}
		return jsonResponse(http.StatusOK, okEnvelope(`{"list":[`+strings.Join(rows, ",")+`]}`))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/points/activation"):
		return jsonResponse(http.StatusOK, okEnvelope(`{"activated":false}`))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/points/activation"):
		var in struct {
			InviteCode string `json:"inviteCode"`
		}
		_ = json.Unmarshal([]byte(body), &in)
		return jsonResponse(http.StatusOK, okEnvelope(fmt.Sprintf(
			`{"activated":true,"alreadyActivated":false,"inviteeReward":5000,"appliedInvitationCode":%q}`, in.InviteCode)))
	default:
		return jsonResponse(http.StatusNotFound, `{}`)
	}
}

func TestInviteCodeDefaultPicksTheCodeWithMostUsesLeft(t *testing.T) {
	stub := &inviteCodesStub{codes: map[string][]inviteCodeUse{
		"tok-a": {{InviteCode: "AAAAAA", UsedCount: 19, MaxUses: 20, Status: "active"}},
		"tok-b": {{InviteCode: "BBBBBB", UsedCount: 5, MaxUses: 20, Status: "active"}},
	}}
	rt := &stubTransport{handler: stub.handler}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "acc-a", "tok-a", 0)
	seedAccount(t, c, "acc-b", "tok-b", 0)

	// acc-b's code has 15 uses left against acc-a's 1, so it wins.
	if got := c.InviteCodeDefault(context.Background()); got != "BBBBBB" {
		t.Fatalf("picked code = %q, want BBBBBB (15 uses left vs 1)", got)
	}

	// Spend acc-b's code; the next scan must fall back to acc-a's remaining 1.
	stub.mu.Lock()
	stub.codes["tok-b"] = []inviteCodeUse{{InviteCode: "BBBBBB", UsedCount: 20, MaxUses: 20, Status: "active"}}
	stub.mu.Unlock()
	if got, _, _ := c.pickInviteCode(context.Background(), ""); got != "AAAAAA" {
		t.Fatalf("picked after spend = %q, want AAAAAA", got)
	}
}

func TestInviteCodeDefaultIgnoresSpentAndInactiveCodes(t *testing.T) {
	stub := &inviteCodesStub{codes: map[string][]inviteCodeUse{
		"tok-a": {
			{InviteCode: "SPENT1", UsedCount: 20, MaxUses: 20, Status: "active"},
			{InviteCode: "OLD001", UsedCount: 0, MaxUses: 20, Status: "expired"},
		},
	}}
	rt := &stubTransport{handler: stub.handler}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "acc-a", "tok-a", 0)

	if got, seen, _ := c.pickInviteCode(context.Background(), ""); got != "" {
		t.Fatalf("picked = %q, want nothing when every code is spent or inactive", got)
	} else if len(seen) != 2 {
		t.Fatalf("saw %d code rows, want both the spent and the inactive one", len(seen))
	}
	// With no usable pool code, the default falls back to the configured one.
	if got := c.InviteCodeDefault(context.Background()); got != "P276D2" {
		t.Fatalf("fallback = %q, want the configured P276D2", got)
	}
}

func TestBindAccountInviteCodeUsesThePoolCodeNotTheCompiledOne(t *testing.T) {
	stub := &inviteCodesStub{codes: map[string][]inviteCodeUse{
		"tok-a": {{InviteCode: "AAAAAA", UsedCount: 19, MaxUses: 20, Status: "active"}},
		"tok-b": {{InviteCode: "BBBBBB", UsedCount: 0, MaxUses: 20, Status: "active"}},
	}}
	rt := &stubTransport{handler: stub.handler}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "acc-a", "tok-a", 0)
	seedAccount(t, c, "acc-b", "tok-b", 0)

	// Binding acc-c (no code of its own) must take acc-b's 20-use code, not
	// the compiled P276D2 and not acc-a's 1-use remainder.
	seedAccount(t, c, "acc-c", "tok-c", 0)
	res, err := c.BindAccountInviteCode(context.Background(), "acc-c", "")
	if err != nil {
		t.Fatalf("BindAccountInviteCode: %v", err)
	}
	if res.Code != "BBBBBB" {
		t.Fatalf("bound code = %q, want BBBBBB from the pool", res.Code)
	}
	if !res.OK || !res.Activated {
		t.Fatalf("result = %+v, want an activated binding", res)
	}

	// The bind itself must exclude the target account: acc-c's own (empty)
	// code list is never a candidate, but the winner comes from acc-b.
	body := rt.sentBodies()[len(rt.sentBodies())-1]
	var sent struct {
		InviteCode string `json:"inviteCode"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("activation body: %v", err)
	}
	if sent.InviteCode != "BBBBBB" {
		t.Fatalf("activation posted inviteCode %q, want BBBBBB", sent.InviteCode)
	}
}

func TestBindAccountInviteCodeAnExplicitCodeBeatsThePicker(t *testing.T) {
	stub := &inviteCodesStub{codes: map[string][]inviteCodeUse{
		"tok-a": {{InviteCode: "AAAAAA", UsedCount: 0, MaxUses: 20, Status: "active"}},
	}}
	rt := &stubTransport{handler: stub.handler}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "acc-a", "tok-a", 0)
	seedAccount(t, c, "acc-b", "tok-b", 0)

	res, err := c.BindAccountInviteCode(context.Background(), "acc-b", "EXPLICIT")
	if err != nil {
		t.Fatalf("BindAccountInviteCode: %v", err)
	}
	if res.Code != "EXPLICIT" {
		t.Fatalf("bound code = %q, want the operator's EXPLICIT", res.Code)
	}
	stub.mu.Lock()
	hits := stub.hits
	stub.mu.Unlock()
	if hits != 0 {
		t.Fatalf("sent %d invitation-codes reads for an explicit code; the picker must not run", hits)
	}
}

func TestInviteCodeDefaultMemoizesThePoolScan(t *testing.T) {
	stub := &inviteCodesStub{codes: map[string][]inviteCodeUse{
		"tok-a": {{InviteCode: "AAAAAA", UsedCount: 0, MaxUses: 20, Status: "active"}},
	}}
	rt := &stubTransport{handler: stub.handler}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "acc-a", "tok-a", 0)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if got := c.InviteCodeDefault(ctx); got != "AAAAAA" {
			t.Fatalf("call %d picked %q, want AAAAAA", i, got)
		}
	}
	stub.mu.Lock()
	hits := stub.hits
	stub.mu.Unlock()
	if hits != 1 {
		t.Fatalf("sent %d invitation-codes reads for 5 default reads; the memo must hold", hits)
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
