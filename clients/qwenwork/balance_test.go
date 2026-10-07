package qwenwork

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

const (
	testAccountContextPath  = "/api/v1/adapter/user/account-context"
	testAccountContextQuery = "user,plan,quota,page,data_sharing"
)

func TestQwenworkOffersBalanceProvider(t *testing.T) {
	qc := checkinClient(t, oneAccount, &fakeTransport{})

	if _, ok := core.AsBalanceProvider(qc); !ok {
		t.Fatal("qwenwork must implement core.BalanceProvider so the panel can show its credit balance")
	}
}

func TestQwenworkAccountBalanceUsesTheOfficialAccountContext(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		return fakeResponse(req, http.StatusOK, `{
			"data": {
				"user": {"id": "u1", "is_biz": false},
				"quota": {
					"user_quota": {
						"total": 2000,
						"used": 766,
						"remaining": 1234,
						"unit": "credits"
					}
				}
			}
		}`), nil
	}
	qc := checkinClient(t, oneAccount, rt)

	bal, err := qc.AccountBalance(context.Background(), "uid:1", time.Hour)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 1234 {
		t.Errorf("credits = %d, want 1234", bal.Credits)
	}
	if bal.Used != 766 {
		t.Errorf("used = %v, want 766", bal.Used)
	}
	if bal.Total != 2000 {
		t.Errorf("total = %d, want 2000", bal.Total)
	}
	if bal.Unit != "积分" {
		t.Errorf("unit = %q, want 积分", bal.Unit)
	}

	if rt.count() != 1 {
		t.Fatalf("made %d requests, want one account-context read", rt.count())
	}
	req := rt.at(0)
	if req.method != http.MethodGet {
		t.Errorf("method = %q, want GET", req.method)
	}
	if !strings.Contains(req.url, testAccountContextPath) {
		t.Errorf("url = %q, want the official account-context path", req.url)
	}
	if got := requestQueryValue(req.url, "include"); got != testAccountContextQuery {
		t.Errorf("include = %q, want %q", got, testAccountContextQuery)
	}
	if got := req.header.Get("Authorization"); got != "Bearer t1" {
		t.Errorf("Authorization = %q, want the account token", got)
	}
	if got := req.header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want application/json", got)
	}
	if got := req.header.Get("User-Agent"); !strings.HasPrefix(got, "qoderwork/") {
		t.Errorf("User-Agent = %q, want qoderwork/<version>", got)
	}
	if got := req.header.Get("X-Request-Id"); got == "" {
		t.Error("X-Request-Id is empty; the vendor client sends one on this endpoint")
	}
}

func TestQwenworkAccountBalanceParsesCamelCaseUserQuota(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		return fakeResponse(req, http.StatusOK, `{
			"data": {
				"user": {"id": "u1"},
				"quota": {"userQuota": {"total": 50, "used": 10}}
			}
		}`), nil
	}
	qc := checkinClient(t, oneAccount, rt)

	bal, err := qc.AccountBalance(context.Background(), "uid:1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 40 || bal.Used != 10 || bal.Total != 50 {
		t.Errorf("balance = %+v, want remaining 40, used 10, total 50", bal)
	}
}

func TestQwenworkAccountBalanceFallsBackToTheQuotaObjectItself(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		return fakeResponse(req, http.StatusOK, `{
			"data": {
				"user": {"id": "u1"},
				"quota": {"total": 100, "used": 25, "remaining": 75, "unit": "points"}
			}
		}`), nil
	}
	qc := checkinClient(t, oneAccount, rt)

	bal, err := qc.AccountBalance(context.Background(), "uid:1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 75 || bal.Used != 25 || bal.Total != 100 {
		t.Errorf("balance = %+v, want remaining 75, used 25, total 100", bal)
	}
	if bal.Unit != "points" {
		t.Errorf("unit = %q, want the vendor's unit", bal.Unit)
	}
}

func TestQwenworkAccountBalanceRejectsAMissingQuota(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		return fakeResponse(req, http.StatusOK, `{"data":{"user":{"id":"u1"}}}`), nil
	}
	qc := checkinClient(t, oneAccount, rt)

	if _, err := qc.AccountBalance(context.Background(), "uid:1", 0); err == nil {
		t.Fatal("a successful reply without quota must be an error, not a fabricated zero balance")
	} else if !strings.Contains(strings.ToLower(err.Error()), "quota") {
		t.Errorf("error = %v, want it to mention the missing quota", err)
	}
}

func TestQwenworkAccountBalanceRenewsTheTokenAndRetriesOnce(t *testing.T) {
	rt := &fakeTransport{}
	seenContext := 0
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "deviceToken") {
			return fakeResponse(req, http.StatusOK, `{"token":"t2"}`), nil
		}
		seenContext++
		if seenContext == 1 {
			return fakeResponse(req, http.StatusUnauthorized, `{"message":"token expired"}`), nil
		}
		return fakeResponse(req, http.StatusOK, `{
			"data": {
				"user": {"id": "u1"},
				"quota": {"user_quota": {"total": 10, "remaining": 8}}
			}
		}`), nil
	}
	qc := checkinClient(t, `{"accounts":[{"uid":"1","access_token":"t1","refresh_token":"r1"}]}`, rt)

	bal, err := qc.AccountBalance(context.Background(), "uid:1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 8 {
		t.Errorf("credits = %d, want 8 after retrying with the renewed token", bal.Credits)
	}
	if rt.count() != 3 {
		t.Fatalf("made %d requests, want account-context(401), refresh, account-context", rt.count())
	}
	if got := rt.at(2).header.Get("Authorization"); got != "Bearer t2" {
		t.Errorf("retry Authorization = %q, want the renewed token", got)
	}
}

func requestQueryValue(rawURL, key string) string {
	i := strings.LastIndex(rawURL, "?")
	if i < 0 {
		return ""
	}
	for _, part := range strings.Split(rawURL[i+1:], "&") {
		k, v, ok := strings.Cut(part, "=")
		if ok && k == key {
			return v
		}
	}
	return ""
}

func TestQwenworkAccountBalanceDoesNotChangePoolLRU(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		return fakeResponse(req, http.StatusOK, `{
			"data": {
				"user": {"id": "u1"},
				"quota": {"user_quota": {"total": 10, "remaining": 8}}
			}
		}`), nil
	}
	qc := checkinClient(t, oneAccount, rt)

	e := qc.pool.find("uid:1")
	qc.pool.mu.Lock()
	e.acct.LastUsed = 1234
	qc.pool.mu.Unlock()

	if _, err := qc.AccountBalance(context.Background(), "uid:1", 0); err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}

	qc.pool.mu.Lock()
	got := e.acct.LastUsed
	qc.pool.mu.Unlock()
	if got != 1234 {
		t.Errorf("LastUsed = %d after a balance read, want the original 1234", got)
	}
}
