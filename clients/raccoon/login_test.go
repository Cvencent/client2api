package raccoon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// --- fixtures -------------------------------------------------------------

// loginUpstream is a stub of the handful of endpoints the login flow touches.
// Every request is recorded so a test can assert on the encrypted phone, the
// qrcode_code rotation and the exact paths.
type loginUpstream struct {
	mu     sync.Mutex
	paths  []string
	bodies []string

	qrStatus      string
	qrAccessToken string
	qrRefresh     string
	smsSendErr    string
	smsLoginToken string
	userName      string
}

func (u *loginUpstream) record(r *http.Request) string {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	u.mu.Lock()
	defer u.mu.Unlock()
	u.paths = append(u.paths, r.URL.Path)
	u.bodies = append(u.bodies, string(raw))
	return string(raw)
}

func (u *loginUpstream) countPath(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, p := range u.paths {
		if p == path {
			n++
		}
	}
	return n
}

func (u *loginUpstream) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u.record(r)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case pathQRLogin:
			status := u.qrStatus
			if status == "" {
				status = "pending"
			}
			data := map[string]any{"status": status}
			if status == "success" {
				data["access_token"] = u.qrAccessToken
				data["refresh_token"] = u.qrRefresh
			}
			if status == "logging" {
				data["expired_at"] = "2026-10-02T10:00:00Z"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
		case pathUserInfo:
			name := u.userName
			if name == "" {
				name = "RaccoonAva"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"id": "user-1", "name": name, "phone": "13800001111", "office_identity": "personal",
			}})
		case pathPointsGrant:
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"granted": true}})
		case pathSendSMS:
			if u.smsSendErr != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 100006, "message": u.smsSendErr})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
		case pathLoginSMS:
			tok := u.smsLoginToken
			if tok == "" {
				tok = "sms-token"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"access_token": tok, "refresh_token": "sms-refresh", "office_identity": "personal",
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 404, "message": "not found"})
		}
	}
}

func newLoginClient(t *testing.T, up *loginUpstream) *Client {
	t.Helper()
	ts := httptest.NewServer(up.handler(t))
	t.Cleanup(ts.Close)
	return newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q}`, ts.URL), nil)
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("GET %s: decoding: %v", url, err)
	}
	return out
}

func getPage(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// --- QR login -------------------------------------------------------------

func TestStartLoginServesALoopbackPage(t *testing.T) {
	c := newLoginClient(t, &loginUpstream{})
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = c.CancelLogin(context.Background(), st.SessionID) })
	if st.State != core.LoginPending {
		t.Fatalf("state = %q, want pending", st.State)
	}
	if !strings.HasPrefix(st.URL, "http://127.0.0.1:") {
		t.Fatalf("URL = %q, want a loopback address", st.URL)
	}
	status, page := getPage(t, st.URL)
	if status != http.StatusOK {
		t.Fatalf("login page answered %d, want 200", status)
	}
	for _, want := range []string{"<svg", "微信扫码", "短信登录", pathPoll, pathSmsSend, pathSmsVerify} {
		if !strings.Contains(page, want) {
			t.Fatalf("login page is missing %q", want)
		}
	}
	if strings.Contains(page, phoneCipherSecret) {
		t.Fatal("the login page leaked the phone cipher secret")
	}
}

func TestLoginPageDoesNotCarryTheQrCode(t *testing.T) {
	c := newLoginClient(t, &loginUpstream{})
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = c.CancelLogin(context.Background(), st.SessionID) })
	sess := c.loginByID(st.SessionID)
	if sess == nil {
		t.Fatal("the session is not registered")
	}
	_, page := getPage(t, st.URL)
	if strings.Contains(page, sess.qrCode) {
		t.Fatal("the login page carries the raw qrcode_code; only the rendered QR may")
	}
}

func TestPollMapsEveryQrStatus(t *testing.T) {
	cases := []struct {
		upstream string
		want     string
	}{
		{"pending", "pending"},
		{"logging", "logging"},
		{"canceled", "canceled"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			up := &loginUpstream{qrStatus: tc.upstream}
			c := newLoginClient(t, up)
			st, err := c.StartLogin(context.Background())
			if err != nil {
				t.Fatalf("StartLogin: %v", err)
			}
			t.Cleanup(func() { _ = c.CancelLogin(context.Background(), st.SessionID) })
			got := getJSON(t, "http://"+mustHost(t, st.URL)+pathPoll)
			if got["status"] != tc.want {
				t.Fatalf("status = %v, want %v", got["status"], tc.want)
			}
		})
	}
}

func TestCanceledQrRotatesTheCode(t *testing.T) {
	up := &loginUpstream{qrStatus: "canceled"}
	c := newLoginClient(t, up)
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = c.CancelLogin(context.Background(), st.SessionID) })
	sess := c.loginByID(st.SessionID)
	first := sess.qrCode

	got := getJSON(t, "http://"+mustHost(t, st.URL)+pathPoll)
	if got["status"] != "canceled" {
		t.Fatalf("status = %v, want canceled", got["status"])
	}
	if qr, _ := got["qr"].(string); !strings.Contains(qr, "<svg") {
		t.Fatal("a canceled poll must hand the page a fresh QR")
	}
	if sess.qrCode == first {
		t.Fatal("the qrcode_code was not rotated after a cancel")
	}
	if up.countPath(pathQRLogin) != 1 {
		t.Fatalf("poll requests = %d, want 1", up.countPath(pathQRLogin))
	}
}

func TestQrSuccessStoresAndPersistsTheAccount(t *testing.T) {
	tok := jwtWith(t, map[string]any{"exp": time.Now().Add(3 * time.Hour).Unix()})
	up := &loginUpstream{qrStatus: "success", qrAccessToken: tok, qrRefresh: "refresh-1"}
	c := newLoginClient(t, up)
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = c.CancelLogin(context.Background(), st.SessionID) })

	got := getJSON(t, "http://"+mustHost(t, st.URL)+pathPoll)
	if got["status"] != "success" {
		t.Fatalf("status = %v, want success", got["status"])
	}
	polled, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if polled.State != core.LoginSuccess || polled.AccountID == "" {
		t.Fatalf("state = %+v, want success with an account", polled)
	}
	e := c.pool.find(polled.AccountID)
	if e == nil {
		t.Fatalf("account %q is not in the pool", polled.AccountID)
	}
	if e.acct.AccessToken != tok || e.acct.RefreshToken != "refresh-1" {
		t.Fatalf("stored credential = %+v, want the tokens the vendor returned", e.acct.credential)
	}
	if e.acct.UserID != "user-1" || e.acct.Nickname != "RaccoonAva" {
		t.Fatalf("account was not enriched from user_info: %+v", e.acct)
	}
	if e.acct.Origin != originStored {
		t.Fatalf("origin = %q, want %q", e.acct.Origin, originStored)
	}
	raw, err := os.ReadFile(filepath.Join(c.deps.DataDir, accountsFile))
	if err != nil {
		t.Fatalf("reading accounts.json: %v", err)
	}
	if !strings.Contains(string(raw), tok) {
		t.Fatal("the credential was not persisted to accounts.json")
	}
	if up.countPath(pathPointsGrant) != 1 {
		t.Fatalf("login-points grant requests = %d, want 1", up.countPath(pathPointsGrant))
	}
}

func TestPollLoginAdvancesTheQrWithoutThePage(t *testing.T) {
	tok := jwtWith(t, map[string]any{"exp": time.Now().Add(3 * time.Hour).Unix()})
	up := &loginUpstream{qrStatus: "success", qrAccessToken: tok, qrRefresh: "refresh-1"}
	c := newLoginClient(t, up)
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = c.CancelLogin(context.Background(), st.SessionID) })

	first, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if first.State != core.LoginSuccess {
		t.Fatalf("state = %q, want success", first.State)
	}
	if up.countPath(pathQRLogin) != 1 {
		t.Fatalf("poll requests = %d, want exactly 1", up.countPath(pathQRLogin))
	}
}

func TestPollLoginBeforeAnyScanStaysPending(t *testing.T) {
	up := &loginUpstream{qrStatus: "pending"}
	c := newLoginClient(t, up)
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = c.CancelLogin(context.Background(), st.SessionID) })
	got, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if got.State != core.LoginPending {
		t.Fatalf("state = %q, want pending", got.State)
	}
}

func TestCancelLoginStopsTheLoopbackServer(t *testing.T) {
	c := newLoginClient(t, &loginUpstream{})
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := c.CancelLogin(context.Background(), st.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}
	if err := c.CancelLogin(context.Background(), st.SessionID); err != nil {
		t.Fatalf("second CancelLogin: %v", err)
	}
	polled, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin after cancel: %v", err)
	}
	if polled.State != core.LoginCancelled {
		t.Fatalf("state = %q, want cancelled", polled.State)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(st.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("the loopback server answered after cancel with %d", resp.StatusCode)
	}
}

func TestUnknownLoginSessionIsRefused(t *testing.T) {
	c := newLoginClient(t, &loginUpstream{})
	if _, err := c.PollLogin(context.Background(), "nope"); err == nil {
		t.Fatal("PollLogin accepted an unknown session id")
	}
	if err := c.CancelLogin(context.Background(), "nope"); err == nil {
		t.Fatal("CancelLogin accepted an unknown session id")
	}
}

// mustHost extracts the host:port from a URL without pulling in net/url.
func mustHost(t *testing.T, raw string) string {
	t.Helper()
	i := strings.Index(raw, "://")
	if i < 0 {
		t.Fatalf("%q is not a URL", raw)
	}
	rest := raw[i+3:]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
