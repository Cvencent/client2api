package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// 钉住三个「optional body」写接口的坏请求契约（issue #194）：
//
//	空 body 走默认语义；坏 JSON 必须 400 且模块侧零调用；超限 413。
//	曾用 `_ = decodeBody(...)` 把两者合并——refreshAccounts 的空 ID 是
//	「刷新全池」，一个被截断的 {"id":"A2"} 会退化成全池上游探测。

// refreshSpy 是记录 RefreshAccount 收到什么 ID 的 AccountManager。
type refreshSpy struct {
	fakeClient
	fakeAccountClient
	gotIDs []string
}

func (s *refreshSpy) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	s.gotIDs = append(s.gotIDs, id)
	return []core.RefreshResult{{AccountID: id}}, nil
}

// CheckinProvider plumbing so the checkin route reaches its body decode.
func (s *refreshSpy) CheckinActions(context.Context) []core.CheckinAction { return nil }
func (s *refreshSpy) Checkin(context.Context, string, string) (core.CheckinResult, error) {
	return core.CheckinResult{}, nil
}

func postBody(t *testing.T, p *panel, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	p.refreshAccounts(rec, req, p.opts.Registry.All()[0])
	return rec
}

func TestRefreshAccountsRejectsABrokenBodyInsteadOfSweepingThePool(t *testing.T) {
	spy := &refreshSpy{fakeClient: fakeClient{name: "wb"},
		fakeAccountClient: fakeAccountClient{accounts: []core.AccountRecord{{ID: "A2"}}}}
	p := taskPanel(t, spy)

	post := func(body string) *httptest.ResponseRecorder {
		return postBody(t, p, "/panel/api/clients/wb/accounts/refresh", body)
	}

	// 空 body：合法，保持「刷新全池」语义（空 ID）。
	if r := post(""); r.Code != http.StatusOK {
		t.Fatalf("empty body = %d (%s), want 200 with the pool-wide semantics", r.Code, r.Body.String())
	}
	// 坏 JSON（截断/乱写）：必须 400 且模块侧零调用。
	for _, bad := range []string{`{"id": "A2"`, `not json at all`} {
		spy.gotIDs = nil
		if r := post(bad); r.Code != http.StatusBadRequest {
			t.Errorf("broken body %q = %d, want 400（坏请求不得退化成全池探测，issue #194）", bad, r.Code)
		}
		if len(spy.gotIDs) != 0 {
			t.Errorf("broken body %q reached the module with ids %v：零调用才是对的（issue #194）", bad, spy.gotIDs)
		}
	}
	// 合法单账号：仍然只刷那一个。
	spy.gotIDs = nil
	if r := post(`{"id": "A2"}`); r.Code != http.StatusOK {
		t.Fatalf("valid single refresh = %d (%s)", r.Code, r.Body.String())
	}
	if len(spy.gotIDs) != 1 || spy.gotIDs[0] != "A2" {
		t.Fatalf("valid refresh ids = %v, want exactly [A2]", spy.gotIDs)
	}
}

func TestTestAccountRejectsABrokenBody(t *testing.T) {
	spy := &refreshSpy{fakeClient: fakeClient{name: "wb"},
		fakeAccountClient: fakeAccountClient{accounts: []core.AccountRecord{{ID: "A1"}}}}
	p := taskPanel(t, spy)

	rec := httptest.NewRecorder()
	reqT := httptest.NewRequest(http.MethodPost, "/panel/api/clients/wb/accounts/A1/test", strings.NewReader(`{"model": "m1"`))
	reqT.Header.Set("Content-Type", "application/json")
	reqT.Host = "127.0.0.1"
	p.testAccount(rec, reqT, spy, "A1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("truncated test body = %d, want 400（模型选择被静默丢弃曾报成功，issue #194）", rec.Code)
	}
}

func TestCheckinRejectsABrokenBody(t *testing.T) {
	spy := &refreshSpy{fakeClient: fakeClient{name: "wb"},
		fakeAccountClient: fakeAccountClient{accounts: []core.AccountRecord{{ID: "A1"}}}}
	p := taskPanel(t, spy)

	rec := httptest.NewRecorder()
	reqC := httptest.NewRequest(http.MethodPost, "/panel/api/clients/wb/accounts/A1/checkin", strings.NewReader(`{bad`))
	reqC.Header.Set("Content-Type", "application/json")
	reqC.Host = "127.0.0.1"
	p.checkin(rec, reqC, spy, "A1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage checkin body = %d, want 400（issue #194）", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid JSON") {
		t.Errorf("error text = %q, want the decodeJSON wording", rec.Body.String())
	}
}
