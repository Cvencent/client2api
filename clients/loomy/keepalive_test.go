package loomy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The keepalive chore is the module's one scheduled session heartbeat.  These
// tests pin the three things that matter: it calls the ACCOUNT service (not the
// business host), a genuine auth code parks the account for a manual re-login,
// and a transient hiccup never does.

func TestKeepaliveHeartbeatPostsTheSessionToTheAccountService(t *testing.T) {
	stub := &stubTransport{handler: func(*http.Request, string) *http.Response {
		return jsonResponse(http.StatusOK, okEnvelope(`{"userid":"123","nickname":"n"}`))
	}}
	c := newTestClient(t, `{"account_base":"https://account.example.test","api_base":"https://api.example.test","access_key_id":"id","access_key_secret":"secret","app_id":"app"}`, stub)
	seedAccount(t, c, "loomy-1", testToken, 0)

	res, err := c.RunTask(context.Background(), "loomy-1", choreKeepalive)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("RunTask keepalive = %+v, want OK", res)
	}

	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want exactly one heartbeat", len(reqs))
	}
	req := reqs[0]
	if req.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", req.Method)
	}
	// It must talk to the account service, never the business host: the
	// business host is already polled every few minutes and sessions still
	// lapsed, so a business ping proves nothing.
	if got, want := req.URL.String(), "https://account.example.test/userinfo/query/baseInfo"; got != want {
		t.Fatalf("url = %s, want %s", got, want)
	}
	if auth := req.Header.Get("Authorization"); !strings.HasPrefix(auth, "account id:") {
		t.Fatalf("Authorization = %q, want an account signature", auth)
	}
	if body := stub.lastBody(); !strings.Contains(body, testToken) {
		t.Fatalf("body %q does not carry the session", body)
	}
}

func TestKeepaliveCoolsDownADeadSessionWithoutParkingIt(t *testing.T) {
	// Both the business code and the account service's own code mean the same
	// thing, but a background heartbeat is not authoritative enough to
	// permanently park a credential a real chat request may still use.
	for _, code := range []string{"100002", "020002"} {
		t.Run(code, func(t *testing.T) {
			stub := alwaysJSON(http.StatusOK, failureEnvelope(code, "登录已失效，请重新登录"))
			c := newTestClient(t, `{}`, stub)
			seedAccount(t, c, "loomy-dead", testToken, 0)

			res, err := c.RunTask(context.Background(), "loomy-dead", choreKeepalive)
			if err != nil {
				t.Fatalf("RunTask: %v", err)
			}
			if res.OK {
				t.Fatalf("RunTask = %+v, want OK false for a dead session", res)
			}
			if !strings.Contains(res.Error, "log in again") {
				t.Fatalf("error = %q, want the re-login wording", res.Error)
			}
			acc, ok := c.store.lookup("loomy-dead")
			if !ok {
				t.Fatal("account vanished")
			}
			if acc.dead {
				t.Fatalf("business code %s permanently parked the account from a heartbeat", code)
			}
			if acc.cooldownTill.IsZero() {
				t.Fatalf("business code %s did not cool the account down", code)
			}
			if acc.selectable(time.Now().UTC()) {
				t.Fatalf("business code %s left the account selectable during cooldown", code)
			}
		})
	}
}

func TestKeepaliveTransportFailureDoesNotCoolTheAccount(t *testing.T) {
	c := newTestClient(t, `{}`, failingTransport{err: errors.New("network down")})
	seedAccount(t, c, "loomy-net", testToken, 0)

	res, err := c.RunTask(context.Background(), "loomy-net", choreKeepalive)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.OK {
		t.Fatal("a transport failure is not a success")
	}
	acc, _ := c.store.lookup("loomy-net")
	if acc.dead {
		t.Fatal("a transport failure must not park the account")
	}
	if !acc.cooldownTill.IsZero() {
		t.Fatal("a background heartbeat hiccup must not cool the chat pool")
	}
}

func TestKeepaliveSkipsAParkedAccount(t *testing.T) {
	stub := &stubTransport{}
	c := newTestClient(t, `{}`, stub)
	seedAccount(t, c, "loomy-parked", testToken, 0)
	c.store.penalise("loomy-parked", errSessionExpired("loomy-parked"), time.Now().UTC(), time.Hour)

	res, err := c.RunTask(context.Background(), "loomy-parked", choreKeepalive)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.Skipped {
		t.Fatalf("RunTask = %+v, want Skipped for a parked account", res)
	}
	if stub.count() != 0 {
		t.Fatalf("a parked account was still probed: %d requests", stub.count())
	}
}

func TestRunTaskStillRefusesUnknownCodes(t *testing.T) {
	c := newTestClient(t, `{}`, alwaysJSON(http.StatusOK, okEnvelope("null")))
	seedAccount(t, c, "loomy-1", testToken, 0)
	if _, err := c.RunTask(context.Background(), "loomy-1", "not-a-chore"); err == nil {
		t.Fatal("an unknown task code must still be an error")
	}
}
