package workbuddy

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// unverified_resolution_test.go pins the active half of WorkBuddy's
// contradictory-balance handling.  A "pending" reading is not something the
// operator should have to resolve by hand: a positive balance is enough to put
// the account back in rotation, while a zero reading has to be checked through
// the same real Auto call the panel's Test button uses.

const unverifiedZeroAccounts = `{
  "PackageName":"trial",
  "CapacityRemain":500,"CapacityUsed":0,"CapacitySize":500,
  "CycleCapacityRemain":0,"CycleCapacityUsed":500,"CycleCapacitySize":500,
  "ExpiredTime":"","CycleEndTime":"2035-01-01 00:00:00"
}`

func unverifiedPositiveAccounts() string {
	return unverifiedZeroAccounts + `,{
	  "PackageName":"paid",
	  "CapacityRemain":120,"CapacityUsed":0,"CapacitySize":120,
	  "CycleEndTime":"2035-01-01 00:00:00"
	}`
}

func countWorkbuddyCalls(t *testing.T, rt *fakeRT, suffix string) int {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	n := 0
	for _, req := range rt.calls {
		if strings.HasSuffix(req.URL.Path, suffix) {
			n++
		}
	}
	return n
}

func TestWorkbuddyPanelAccountBalanceResolvesUnverifiedPositiveCreditWithoutProbe(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, chatCompletionsPath) {
			t.Fatalf("a positive unverified balance must not spend a chat probe: %s", req.URL)
		}
		return jsonResponse(http.StatusOK, meterEnvelope(unverifiedPositiveAccounts())), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	got, err := c.AccountBalance(context.Background(), "uid-cn-0001", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if got.Credits != 120 {
		t.Fatalf("credits = %d, want 120", got.Credits)
	}
	if got.Unverified {
		t.Fatalf("positive unverified balance stayed pending: %+v", got)
	}
	if n := countWorkbuddyCalls(t, rt, chatCompletionsPath); n != 0 {
		t.Fatalf("chat probes = %d, want 0", n)
	}
	if st := statusOf(t, c.pool, "uid-cn-0001"); st.State != stateReady {
		t.Fatalf("state = %q, want %q", st.State, stateReady)
	}
}

func TestWorkbuddyPanelAccountBalanceProbesUnverifiedZeroAndMarksReady(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, chatCompletionsPath) {
			return sseResponse(http.StatusOK, autoProbeSSEFixture, nil), nil
		}
		return jsonResponse(http.StatusOK, meterEnvelope(unverifiedZeroAccounts)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	got, err := c.AccountBalance(context.Background(), "uid-cn-0001", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if got.Unverified {
		t.Fatalf("probed zero balance stayed pending: %+v", got)
	}
	if n := countWorkbuddyCalls(t, rt, chatCompletionsPath); n != 1 {
		t.Fatalf("chat probes = %d, want exactly 1", n)
	}
	if st := statusOf(t, c.pool, "uid-cn-0001"); st.State != stateReady {
		t.Fatalf("state = %q, want %q", st.State, stateReady)
	}
}

func TestWorkbuddyPanelAccountBalanceProbesUnverifiedZeroAndClassifiesFailure(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, chatCompletionsPath) {
			return jsonResponse(http.StatusPaymentRequired, `{"code":1,"msg":"积分不足"}`), nil
		}
		return jsonResponse(http.StatusOK, meterEnvelope(unverifiedZeroAccounts)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	got, err := c.AccountBalance(context.Background(), "uid-cn-0001", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if got.Unverified {
		t.Fatalf("failed probe stayed pending: %+v", got)
	}
	if n := countWorkbuddyCalls(t, rt, chatCompletionsPath); n != 1 {
		t.Fatalf("chat probes = %d, want exactly 1", n)
	}
	if st := statusOf(t, c.pool, "uid-cn-0001"); st.State != stateExhausted {
		t.Fatalf("state = %q, want %q", st.State, stateExhausted)
	}
}

func TestWorkbuddyPanelAccountBalanceDoesNotReprobeUnverifiedZeroWithinWindow(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, chatCompletionsPath) {
			return sseResponse(http.StatusOK, autoProbeSSEFixture, nil), nil
		}
		return jsonResponse(http.StatusOK, meterEnvelope(unverifiedZeroAccounts)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	for i := 0; i < 2; i++ {
		if _, err := c.AccountBalance(context.Background(), "uid-cn-0001", 0); err != nil {
			t.Fatalf("AccountBalance #%d: %v", i+1, err)
		}
		time.Sleep(time.Millisecond)
	}
	if n := countWorkbuddyCalls(t, rt, chatCompletionsPath); n != 1 {
		t.Fatalf("chat probes = %d, want no more than one inside the probe window", n)
	}
}
