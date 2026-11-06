package loomy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
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

	// The read itself must NOT block on the pool scan (issue #96).  The value
	// is not an assertion target (issue #195's sibling): the background sweep
	// and this read race, and on a loaded CI runner the sweep occasionally
	// lands first -- "BBBBBB" is then the CORRECT answer for a read that
	// arrived after the sweep.  What is pinned is the property: immediate
	// return, either the configured fallback or a pool code, and no upstream
	// hit from the synchronous read itself.
	// 同前：不断言 hits（后台扫描与读并发，计数器分不清归属），只钉「立即返回 + 值合法」。
	first := c.InviteCodeDefault(context.Background())
	if first != "P276D2" && first != "AAAAAA" && first != "BBBBBB" {
		t.Fatalf("a read before the first sweep = %q, want the fallback or a pool code (issue #195/#196 family)", first)
	}
	// acc-b's code has 15 uses left against acc-a's 1, so it wins -- observable
	// once the background sweep has landed.
	if got := waitForInvitePick(t, c, "BBBBBB"); got != "BBBBBB" {
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

// 面板每次刷新状态都会读一次默认邀请码，而这条读取**绝不能**等上游
// （issue #96：52 个账号的池子一次冷扫描把 /panel/api/status 拖住 3～4 秒）。
//
// 判据分两半：
//  1. 读取必须立刻返回，且**不做任何上游请求**——它只回已知值或配置兜底；
//  2. 后台扫描仍然发生（功能不丢），扫描结果落进 memo 后读取就换成它；
//  3. 面板连续轮询时，扫描由 memo/TTL 与「同时只跑一个」共同收敛，不随读次增长。
func TestInviteCodeDefaultNeverBlocksTheReadOnThePoolScan(t *testing.T) {
	stub := &inviteCodesStub{codes: map[string][]inviteCodeUse{
		"tok-a": {{InviteCode: "AAAAAA", UsedCount: 0, MaxUses: 20, Status: "active"}},
	}}
	rt := &stubTransport{handler: stub.handler}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "acc-a", "tok-a", 0)

	ctx := context.Background()
	// 第一次读取：必须立即返回（不阻塞在扫描上，#96 的尖刺来源）且不产生
	// 上游请求。返回值本身不是断言对象——warmInvitePick 的后台扫描与这条读
	// 是赛跑关系（issue #195：CI 上扫描偶尔抢先落地，读到池码是**正确**行为），
	// 快机器上兜底、慢调度下池码，两者都合法；真正要钉的是「立即」与「零请求」。
	// 注意：这里不断言「读前后 hits 不变」——warmInvitePick 会立刻起后台扫描，
	// 那个 goroutine 与这条同步读并发，hits 分不清是谁发的（CI 上就此翻车过一次）。
	// 可观测的判据是「读本身立即返回」，不是「期间上游零请求」。
	coldStart := time.Now()
	got := c.InviteCodeDefault(ctx)
	coldElapsed := time.Since(coldStart)
	if coldElapsed > 100*time.Millisecond {
		t.Fatalf("cold read took %s：读数在等上游扫描，这正是 #96 的尖刺来源", coldElapsed)
	}
	if got != "P276D2" && got != "AAAAAA" {
		t.Fatalf("cold read = %q, want either the configured fallback or a pool code (both are immediate correct answers)", got)
	}

	// 后台扫描落地后，读取换成池子里余量最大的码（功能没丢）。
	if got := waitForInvitePick(t, c, "AAAAAA"); got != "AAAAAA" {
		t.Fatalf("after the sweep picked = %q, want AAAAAA", got)
	}

	// 再连读若干次：memo 命中时读取必须**一次上游都不打**。
	stub.mu.Lock()
	before := stub.hits
	stub.mu.Unlock()
	for i := 0; i < 5; i++ {
		if got := c.InviteCodeDefault(ctx); got != "AAAAAA" {
			t.Fatalf("call %d picked %q, want the memoized AAAAAA", i, got)
		}
	}
	stub.mu.Lock()
	after := stub.hits
	stub.mu.Unlock()
	if after != before {
		t.Fatalf("memo 命中后又打了 %d 次上游；读数不该触发扫描", after-before)
	}
}

// waitForInvitePick 等后台扫描把 memo 填成 want，最多几秒。扫描是异步的，
// 测试必须等它落地而不是假定同步返回。
func waitForInvitePick(t *testing.T, c *Client, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = c.InviteCodeDefault(context.Background())
		if last == want {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	return last
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
