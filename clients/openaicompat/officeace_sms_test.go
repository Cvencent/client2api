package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// fakeOfficeAceSMS is a minimal eomsg server.  The platform protocol is plain
// text, so driving the real smscap client keeps these tests honest without
// touching the network.
type fakeOfficeAceSMS struct {
	mu       sync.Mutex
	phones   []string
	draws    int
	msg      string
	releases []string
	blocks   []string
	calls    []url.Values
}

func (f *fakeOfficeAceSMS) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, q)

	switch q.Get("code") {
	case "leftAmount":
		fmt.Fprint(w, "28.55")
	case "getPhone":
		if want := strings.TrimSpace(q.Get("phone")); want != "" {
			fmt.Fprint(w, want)
			return
		}
		if len(f.phones) == 0 {
			fmt.Fprint(w, "ERROR no numbers")
			return
		}
		i := f.draws
		f.draws++
		if i >= len(f.phones) {
			i = len(f.phones) - 1
		}
		fmt.Fprint(w, f.phones[i])
	case "getMsg":
		if strings.TrimSpace(f.msg) == "" {
			fmt.Fprint(w, "[尚未收到]")
			return
		}
		fmt.Fprint(w, f.msg)
	case "release":
		f.releases = append(f.releases, q.Get("phone"))
		fmt.Fprint(w, "SUCCESS")
	case "block":
		f.blocks = append(f.blocks, q.Get("phone"))
		fmt.Fprint(w, "SUCCESS")
	default:
		fmt.Fprint(w, "ERROR unknown code")
	}
}

func (f *fakeOfficeAceSMS) snapshot() (draws int, releases, blocks []string, calls []url.Values) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.draws, append([]string(nil), f.releases...), append([]string(nil), f.blocks...), append([]url.Values(nil), f.calls...)
}

// officeAceTestSource builds an OfficeAce wrapper over a source whose HTTP
// client reaches the given test servers.  smsBase is the per-test SMS endpoint;
// the real deployment uses the shipped eomsg default.
func officeAceTestSource(t *testing.T, deps core.Deps, smsBase string) *officeAceSource {
	t.Helper()
	if deps.DataDir == "" {
		deps.DataDir = t.TempDir()
	}
	s, err := newSource(officeAceSourceID, core.SourceConfig{BaseURL: "https://default.invalid/v2"}, deps)
	if err != nil {
		t.Fatalf("newSource: %v", err)
	}
	return &officeAceSource{source: s, smsBase: smsBase}
}

func TestOfficeAceAdvertisesSMSAndAutoLogin(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	o := officeAceTestSource(t, core.Deps{}, "")
	caps := core.CapabilitiesOf(context.Background(), o)
	if !caps.SMS {
		t.Fatalf("capabilities = %+v, want SMS for the Huawei phone-code login", caps)
	}
	if !caps.AutoLogin {
		t.Fatalf("capabilities = %+v, want AutoLogin for the Huawei phone-code login", caps)
	}
	if !caps.Login {
		t.Fatalf("capabilities = %+v, want the ordinary account login kept", caps)
	}
}

func TestOfficeAceSMSStatusDefaultsToHuaweiKeyword(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	o := officeAceTestSource(t, core.Deps{}, "")
	st := o.SMSStatus(context.Background(), core.SMSOpts{})
	if st.Configured {
		t.Fatalf("status = %+v, want unconfigured without a token", st)
	}
	if st.Keyword != officeAceDefaultSMSKey {
		t.Fatalf("keyword = %q, want %q", st.Keyword, officeAceDefaultSMSKey)
	}
	if st.Provider != officeAceSMSProvider {
		t.Fatalf("provider = %q, want %q", st.Provider, officeAceSMSProvider)
	}
}

func TestOfficeAceSMSStatusReadsBalance(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	f := &fakeOfficeAceSMS{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, srv.URL)

	st := o.SMSStatus(context.Background(), core.SMSOpts{Token: "tok-abc"})
	if !st.Configured || st.Balance != "28.55" || st.Error != "" {
		t.Fatalf("status = %+v, want the platform balance", st)
	}
	_, _, _, calls := f.snapshot()
	if len(calls) != 1 || calls[0].Get("code") != "leftAmount" {
		t.Fatalf("calls = %v, want one balance read", calls)
	}
	if calls[0].Get("token") != "tok-abc" {
		t.Fatalf("token was not sent to the platform")
	}
}

func TestOfficeAceSMSStatusRequiresAToken(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	f := &fakeOfficeAceSMS{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, srv.URL)

	st := o.SMSStatus(context.Background(), core.SMSOpts{})
	if st.Configured {
		t.Fatalf("status = %+v, want unconfigured", st)
	}
	if _, _, _, calls := f.snapshot(); len(calls) != 0 {
		t.Fatalf("calls = %v, want no unauthenticated platform request", calls)
	}
}

func TestOfficeAceAcquirePhoneSkipsAvoidedNumbers(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	f := &fakeOfficeAceSMS{phones: []string{"17000000001", "17000000002", "13800000000"}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, srv.URL)

	num, err := o.AcquirePhone(context.Background(), core.SMSOpts{Token: "tok-abc"}, "", []string{"17000000001", "17000000002"})
	if err != nil {
		t.Fatalf("AcquirePhone: %v", err)
	}
	if num.Phone != "13800000000" {
		t.Fatalf("phone = %q, want the first number not in avoid", num.Phone)
	}
	if num.Reused {
		t.Fatalf("number = %+v, want a fresh draw", num)
	}
	if _, releases, _, _ := f.snapshot(); len(releases) != 2 {
		t.Fatalf("released = %v, want every avoided draw handed straight back", releases)
	}
}

func TestOfficeAceAcquirePhoneFailsAfterEveryDrawIsAvoided(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	f := &fakeOfficeAceSMS{phones: []string{"17000000001"}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, srv.URL)

	_, err := o.AcquirePhone(context.Background(), core.SMSOpts{Token: "tok-abc"}, "", []string{"17000000001"})
	if err == nil {
		t.Fatal("AcquirePhone accepted an avoided number")
	}
	draws, releases, _, _ := f.snapshot()
	if draws != officeAceDupRetries || len(releases) != officeAceDupRetries {
		t.Fatalf("draws=%d releases=%d, want %d of each", draws, len(releases), officeAceDupRetries)
	}
}

func TestOfficeAceAcquirePhoneReissuesPinnedNumber(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	f := &fakeOfficeAceSMS{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, srv.URL)

	num, err := o.AcquirePhone(context.Background(), core.SMSOpts{Token: "tok-abc"}, "17000000001", nil)
	if err != nil {
		t.Fatalf("AcquirePhone: %v", err)
	}
	if num.Phone != "17000000001" || !num.Reused {
		t.Fatalf("number = %+v, want the pinned account number marked reused", num)
	}
}

func TestOfficeAceAcquirePhoneRequiresAToken(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	f := &fakeOfficeAceSMS{phones: []string{"13800000000"}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, srv.URL)

	if _, err := o.AcquirePhone(context.Background(), core.SMSOpts{}, "", nil); err == nil {
		t.Fatal("AcquirePhone worked without a platform token")
	} else if !strings.Contains(err.Error(), "token") {
		t.Fatalf("error = %q, want it to name the missing token", err)
	}
	if _, _, _, calls := f.snapshot(); len(calls) != 0 {
		t.Fatalf("calls = %v, want no unauthenticated platform request", calls)
	}
}

func TestOfficeAceSMSKeywordOverrideReachesThePlatform(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	f := &fakeOfficeAceSMS{phones: []string{"13800000000"}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, srv.URL)

	if _, err := o.AcquirePhone(context.Background(), core.SMSOpts{Token: "tok-abc", Keyword: "华为云"}, "", nil); err != nil {
		t.Fatalf("AcquirePhone: %v", err)
	}
	_, _, _, calls := f.snapshot()
	if len(calls) != 1 || calls[0].Get("keyWord") != "华为云" {
		t.Fatalf("calls = %v, want the per-call keyword override", calls)
	}
}

func TestOfficeAceSMSPollParsesHuaweiMessage(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	f := &fakeOfficeAceSMS{msg: "【华为】您的验证码是 654321，5 分钟内有效。"}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, srv.URL)

	got, err := o.PollSMSCode(context.Background(), core.SMSOpts{Token: "tok-abc"}, "13800000000")
	if err != nil {
		t.Fatalf("PollSMSCode: %v", err)
	}
	if !got.Ready || got.Code != "654321" {
		t.Fatalf("code = %+v, want 654321", got)
	}
}

func TestOfficeAceSMSPollWaitsWhenNothingArrived(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	f := &fakeOfficeAceSMS{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, srv.URL)

	got, err := o.PollSMSCode(context.Background(), core.SMSOpts{Token: "tok-abc"}, "13800000000")
	if err != nil {
		t.Fatalf("PollSMSCode: %v", err)
	}
	if got.Ready {
		t.Fatalf("code = %+v, want the waiting state", got)
	}
}

func TestOfficeAceReleasePhoneBlocksOnRequest(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	f := &fakeOfficeAceSMS{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, srv.URL)

	if err := o.ReleasePhone(context.Background(), core.SMSOpts{Token: "tok-abc"}, "13800000000", false); err != nil {
		t.Fatalf("ReleasePhone: %v", err)
	}
	if err := o.ReleasePhone(context.Background(), core.SMSOpts{Token: "tok-abc"}, "13800000001", true); err != nil {
		t.Fatalf("ReleasePhone(block): %v", err)
	}
	_, releases, blocks, _ := f.snapshot()
	if len(releases) != 1 || releases[0] != "13800000000" {
		t.Fatalf("releases = %v", releases)
	}
	if len(blocks) != 1 || blocks[0] != "13800000001" {
		t.Fatalf("blocks = %v, want the block call routed to the blacklist verb", blocks)
	}
}

func TestOfficeAceSMSProxyOverrideIsValidated(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	o := officeAceTestSource(t, core.Deps{}, "http://example.invalid")
	if _, err := o.AcquirePhone(context.Background(), core.SMSOpts{Token: "tok-abc", Proxy: "http://[::1"}, "", nil); err == nil {
		t.Fatal("AcquirePhone accepted a malformed proxy")
	} else if !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("error = %q, want it to name the proxy", err)
	}
}

func TestOfficeAceSMSEnvTokenIsTheFallback(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "env-token")
	f := &fakeOfficeAceSMS{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, srv.URL)

	st := o.SMSStatus(context.Background(), core.SMSOpts{})
	if !st.Configured || st.Error != "" {
		t.Fatalf("status = %+v, want the environment token accepted", st)
	}
	_, _, _, calls := f.snapshot()
	if len(calls) != 1 || calls[0].Get("token") != "env-token" {
		t.Fatalf("calls = %v, want the EOMSG_TOKEN fallback used", calls)
	}
}

func TestOfficeAceSMSStatusRedactsTheTokenFromErrors(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ERROR bad token secret-tok")
	}))
	defer srv.Close()
	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, srv.URL)

	st := o.SMSStatus(context.Background(), core.SMSOpts{Token: "secret-tok"})
	if strings.Contains(st.Error, "secret-tok") {
		t.Fatalf("error leaked the platform token: %q", st.Error)
	}
}

func TestOfficeAceAutoLoginDrivesBrowserWithSMS(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	sms := &fakeOfficeAceSMS{phones: []string{"13800000000"}, msg: "【华为】验证码：246810，请勿泄露。"}
	smsSrv := httptest.NewServer(http.HandlerFunc(sms.serve))
	defer smsSrv.Close()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/claw/auth/state":
			_, _ = io.WriteString(w, `{"state":"auto-state"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/claw/auth/code":
			_, _ = io.WriteString(w, `{"code":"auto-code","state":"auto-state"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth2/tokens":
			body := map[string]any{
				"credentials": map[string]string{
					"access_key_id":     "test-access-key",
					"secret_access_key": "test-secret-key",
					"security_token":    "test-security-token",
					"project_id":        "test-project",
					"expiration":        "2026-10-16T13:00:00Z",
				},
				"id_token": officeAceTestJWT(t, map[string]any{"sub": "test-subject", "preferred_username": "test-user"}),
			}
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/claw/client-permission-validate":
			_, _ = io.WriteString(w, `{"data":{"account_id":"acct-auto","principal_id":"user-auto","principal_urn":"iam::123:user:auto-user","model_info":{"model_api_url_base":"`+srv.URL+`/model-gateway","model_auth_info":{"model_app_key":"`+testOfficeAceAppKey+`","model_app_secret":"`+testOfficeAceAppSecret+`"}}}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/model-gateway/v2/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"test-glm"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, smsSrv.URL)
	o.eps = officeAceEndpoints{ClawBase: srv.URL, AuthBase: srv.URL, IAMBase: srv.URL, KeystoneBase: srv.URL, ClientID: "test-client"}
	drv := &fakeOfficeAceDriver{code: "246810"}
	o.autoDriver = drv

	job, err := o.StartAutoLogin(context.Background(), core.AutoLoginRequest{Token: "tok-abc"})
	if err != nil {
		t.Fatalf("StartAutoLogin: %v", err)
	}
	if job.State != core.AutoLoginRunning {
		t.Fatalf("job = %+v, want running", job)
	}
	done := waitOfficeAceJob(t, o, job.ID)
	if done.State != core.AutoLoginSuccess {
		t.Fatalf("job = %+v, want success", done)
	}
	if done.AccountID == "" || done.Phone != "13800000000" {
		t.Fatalf("job = %+v, want the account and rented phone reported", done)
	}

	drv.mu.Lock()
	gotURL, gotPhone, gotCode := drv.url, drv.phone, drv.codeSeen
	drv.mu.Unlock()
	officeAceAssertLogoutWrappedLoginURL(t, gotURL)
	if gotPhone != "13800000000" {
		t.Fatalf("driver phone = %q", gotPhone)
	}
	if gotCode != "246810" {
		t.Fatalf("driver code = %q, want the SMS code handed to the page", gotCode)
	}

	_, releases, _, _ := sms.snapshot()
	if len(releases) != 1 || releases[0] != "13800000000" {
		t.Fatalf("releases = %v, want the freshly drawn number handed back", releases)
	}
	o.source.mu.Lock()
	rows := len(o.source.rows)
	o.source.mu.Unlock()
	if rows != 1 {
		t.Fatalf("stored accounts = %d, want the auto login to persist one", rows)
	}
}

func TestOfficeAceAutoLoginKeepsPinnedNumber(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	sms := &fakeOfficeAceSMS{msg: "【华为】验证码：135790。"}
	smsSrv := httptest.NewServer(http.HandlerFunc(sms.serve))
	defer smsSrv.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/claw/auth/state" {
			_, _ = io.WriteString(w, `{"state":"auto-state"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, smsSrv.URL)
	o.eps = officeAceEndpoints{ClawBase: srv.URL, AuthBase: srv.URL, IAMBase: srv.URL, KeystoneBase: srv.URL, ClientID: "test-client"}
	drv := &fakeOfficeAceDriver{code: "135790", failAfter: true}
	o.autoDriver = drv

	job, err := o.StartAutoLogin(context.Background(), core.AutoLoginRequest{Token: "tok-abc", Phone: "17000000001"})
	if err != nil {
		t.Fatalf("StartAutoLogin: %v", err)
	}
	done := waitOfficeAceJob(t, o, job.ID)
	if done.State != core.AutoLoginFailed {
		t.Fatalf("job = %+v, want a failed run", done)
	}
	_, releases, _, _ := sms.snapshot()
	if len(releases) != 0 {
		t.Fatalf("releases = %v, want the account's pinned number left alone", releases)
	}
}

func TestOfficeAceAutoLoginReportsDriverFailure(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	sms := &fakeOfficeAceSMS{phones: []string{"13800000000"}}
	smsSrv := httptest.NewServer(http.HandlerFunc(sms.serve))
	defer smsSrv.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/claw/auth/state" {
			_, _ = io.WriteString(w, `{"state":"auto-state"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, smsSrv.URL)
	o.eps = officeAceEndpoints{ClawBase: srv.URL, AuthBase: srv.URL, IAMBase: srv.URL, KeystoneBase: srv.URL, ClientID: "test-client"}
	o.autoDriver = &fakeOfficeAceDriver{driveErr: fmt.Errorf("page crashed")}

	job, err := o.StartAutoLogin(context.Background(), core.AutoLoginRequest{Token: "tok-abc"})
	if err != nil {
		t.Fatalf("StartAutoLogin: %v", err)
	}
	done := waitOfficeAceJob(t, o, job.ID)
	if done.State != core.AutoLoginFailed || !strings.Contains(done.Message, "page crashed") {
		t.Fatalf("job = %+v, want the driver failure surfaced", done)
	}
}

func TestOfficeAceAutoLoginFailsFastWithoutAToken(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	o := officeAceTestSource(t, core.Deps{}, "http://example.invalid")
	if _, err := o.StartAutoLogin(context.Background(), core.AutoLoginRequest{}); err == nil {
		t.Fatal("StartAutoLogin accepted a run without a platform token")
	}
}

func TestOfficeAceAutoLoginCancelStopsTheRun(t *testing.T) {
	t.Setenv("EOMSG_TOKEN", "")
	sms := &fakeOfficeAceSMS{phones: []string{"13800000000"}}
	smsSrv := httptest.NewServer(http.HandlerFunc(sms.serve))
	defer smsSrv.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/claw/auth/state" {
			_, _ = io.WriteString(w, `{"state":"auto-state"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	o := officeAceTestSource(t, core.Deps{HTTPClient: srv.Client()}, smsSrv.URL)
	o.eps = officeAceEndpoints{ClawBase: srv.URL, AuthBase: srv.URL, IAMBase: srv.URL, KeystoneBase: srv.URL, ClientID: "test-client"}
	drv := &fakeOfficeAceDriver{block: true, entered: make(chan struct{})}
	o.autoDriver = drv

	job, err := o.StartAutoLogin(context.Background(), core.AutoLoginRequest{Token: "tok-abc"})
	if err != nil {
		t.Fatalf("StartAutoLogin: %v", err)
	}
	select {
	case <-drv.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the driver was never reached")
	}
	if err := o.CancelAutoLogin(context.Background(), job.ID); err != nil {
		t.Fatalf("CancelAutoLogin: %v", err)
	}
	done := waitOfficeAceJob(t, o, job.ID)
	if done.State != core.AutoLoginCancelled {
		t.Fatalf("job = %+v, want cancelled", done)
	}
}

// fakeOfficeAceDriver records what the browser step was asked to do and lets
// each test decide how the page behaves.  It never launches a browser.
type fakeOfficeAceDriver struct {
	mu        sync.Mutex
	url       string
	phone     string
	codeSeen  string
	code      string
	driveErr  error
	failAfter bool
	block     bool
	entered   chan struct{}
}

func (d *fakeOfficeAceDriver) Drive(ctx context.Context, url, phone string, waitCode func(context.Context) (string, error), logf func(string, ...any)) error {
	d.mu.Lock()
	d.url = url
	d.phone = phone
	d.mu.Unlock()
	if d.entered != nil {
		close(d.entered)
	}
	if d.driveErr != nil {
		return d.driveErr
	}
	if d.block {
		<-ctx.Done()
		return ctx.Err()
	}
	code, err := waitCode(ctx)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.codeSeen = code
	d.mu.Unlock()
	if d.failAfter {
		return fmt.Errorf("login page rejected the code")
	}
	return nil
}

func waitOfficeAceJob(t *testing.T, o *officeAceSource, id string) core.AutoLoginJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		job, err := o.PollAutoLogin(context.Background(), id)
		if err != nil {
			t.Fatalf("PollAutoLogin: %v", err)
		}
		if job.State != core.AutoLoginRunning {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s stayed running: %+v", id, job)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
