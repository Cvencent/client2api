package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// fakeSMSClient opts a plain panel client into core.SMSProvider and records
// what the routes asked it to do.
type fakeSMSClient struct {
	*fakeClient

	status     core.SMSStatus
	number     core.SMSNumber
	code       core.SMSCode
	acquireErr error

	acquired []string
	avoided  [][]string
	polled   []string
	released []string
	blocked  []string
}

func (f *fakeSMSClient) SMSStatus(context.Context, core.SMSOpts) core.SMSStatus { return f.status }

func (f *fakeSMSClient) AcquirePhone(_ context.Context, _ core.SMSOpts, want string, avoid []string) (core.SMSNumber, error) {
	f.acquired = append(f.acquired, want)
	f.avoided = append(f.avoided, avoid)
	if f.acquireErr != nil {
		return core.SMSNumber{}, f.acquireErr
	}
	n := f.number
	if n.Phone == "" {
		n.Phone = "17000000000"
	}
	return n, nil
}

func (f *fakeSMSClient) PollSMSCode(_ context.Context, _ core.SMSOpts, phone string) (core.SMSCode, error) {
	f.polled = append(f.polled, phone)
	return f.code, nil
}

func (f *fakeSMSClient) ReleasePhone(_ context.Context, _ core.SMSOpts, phone string, block bool) error {
	if block {
		f.blocked = append(f.blocked, phone)
		return nil
	}
	f.released = append(f.released, phone)
	return nil
}

func smsPanel(t *testing.T, clients ...core.Client) http.Handler {
	t.Helper()
	return loginPanel(t, clients...)
}

// TestSMSStatusIsReportedThroughTheCapability: the panel draws the 接码 block
// from this route, so the module's own status must come back verbatim.
func TestSMSStatusIsReportedThroughTheCapability(t *testing.T) {
	c := &fakeSMSClient{
		fakeClient: &fakeClient{name: "wb"},
		status: core.SMSStatus{
			Configured: true,
			Provider:   "eomsg",
			Keyword:    "腾讯科技",
			Balance:    "28.55",
			Provinces:  []string{"广东", "浙江"},
		},
	}
	h := smsPanel(t, c)

	rec := hitRoute(t, h, http.MethodGet, "/panel/api/clients/wb/sms", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)
	if body["configured"] != true || body["balance"] != "28.55" || body["provider"] != "eomsg" {
		t.Errorf("status = %v, want the module's own report", body)
	}

	// The capability matrix has to agree, or the panel would hide the block.
	caps := decodeMap(t, hitRoute(t, h, http.MethodGet, "/panel/api/clients/wb/capabilities", ""))
	if sms, _ := caps["sms"].(bool); !sms {
		t.Errorf("capabilities did not report sms: %v", caps)
	}
}

// TestSMSPhonePassesTheRequestThrough: rent a fresh number, and pass the
// caller's avoid list and a pasted token through untouched.
func TestSMSPhonePassesTheRequestThrough(t *testing.T) {
	c := &fakeSMSClient{fakeClient: &fakeClient{name: "wb"}, number: core.SMSNumber{Phone: "17000000042", Province: "广东"}}
	h := smsPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/wb/sms/phone",
		`{"token":"tok-1","keyword":"腾讯科技","province":"广东","avoid":["17000000001"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)
	if body["phone"] != "17000000042" || body["province"] != "广东" {
		t.Errorf("phone body = %v", body)
	}
	if len(c.acquired) != 1 || c.acquired[0] != "" {
		t.Errorf("acquired = %v, want one empty (fresh) request", c.acquired)
	}
	if len(c.avoided) != 1 || len(c.avoided[0]) != 1 || c.avoided[0][0] != "17000000001" {
		t.Errorf("avoid list = %v, want [17000000001]", c.avoided)
	}
}

// TestSMSPhoneCanReissueAKnownNumber: the restore path asks for one specific
// number, which must arrive as the want argument rather than as a fresh draw.
func TestSMSPhoneCanReissueAKnownNumber(t *testing.T) {
	c := &fakeSMSClient{fakeClient: &fakeClient{name: "wb"}}
	h := smsPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/wb/sms/phone", `{"phone":"13800000000"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(c.acquired) != 1 || c.acquired[0] != "13800000000" {
		t.Errorf("acquired = %v, want the requested number", c.acquired)
	}
}

// TestSMSCodeReportsTheWaitingStateAsSuccess: "not yet" is a 200 with
// ready=false, not an error -- the panel keeps polling on it.
func TestSMSCodeReportsTheWaitingStateAsSuccess(t *testing.T) {
	c := &fakeSMSClient{fakeClient: &fakeClient{name: "wb"}, code: core.SMSCode{Ready: false}}
	h := smsPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/wb/sms/code", `{"phone":"17000000000"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)
	if body["ready"] != false {
		t.Errorf("ready = %v, want false", body["ready"])
	}
	if len(c.polled) != 1 || c.polled[0] != "17000000000" {
		t.Errorf("polled = %v", c.polled)
	}

	// A missing phone is the caller's mistake and must be a 400.
	bad := hitRoute(t, h, http.MethodPost, "/panel/api/clients/wb/sms/code", `{}`)
	if bad.Code != http.StatusBadRequest {
		t.Errorf("missing phone: status = %d, want 400", bad.Code)
	}
}

// TestSMSReleaseChoosesBlockOverRelease pins the two lifecycle verbs.
func TestSMSReleaseChoosesBlockOverRelease(t *testing.T) {
	c := &fakeSMSClient{fakeClient: &fakeClient{name: "wb"}}
	h := smsPanel(t, c)

	if rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/wb/sms/release", `{"phone":"17000000001"}`); rec.Code != http.StatusOK {
		t.Fatalf("release status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/wb/sms/release", `{"phone":"17000000002","block":true}`); rec.Code != http.StatusOK {
		t.Fatalf("block status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(c.released) != 1 || c.released[0] != "17000000001" {
		t.Errorf("released = %v", c.released)
	}
	if len(c.blocked) != 1 || c.blocked[0] != "17000000002" {
		t.Errorf("blocked = %v", c.blocked)
	}
}

// TestSMSRoutesAnswer501WithoutTheCapability: no button for a module that
// cannot rent a number, and no silent success either.
func TestSMSRoutesAnswer501WithoutTheCapability(t *testing.T) {
	h := smsPanel(t, &fakeClient{name: "plain"})
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/panel/api/clients/plain/sms", ""},
		{http.MethodPost, "/panel/api/clients/plain/sms/phone", `{}`},
		{http.MethodPost, "/panel/api/clients/plain/sms/code", `{"phone":"1"}`},
		{http.MethodPost, "/panel/api/clients/plain/sms/release", `{"phone":"1"}`},
	} {
		rec := hitRoute(t, h, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: status = %d, want 501 (body %s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// TestSMSPlatformFailureIsRedacted: the platform error must reach the operator
// without the token that produced it.
func TestSMSPlatformFailureIsRedacted(t *testing.T) {
	c := &fakeSMSClient{
		fakeClient: &fakeClient{name: "wb"},
		acquireErr: errString("the SMS platform refused the request: token=tok-secret-123 is invalid"),
	}
	h := smsPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/wb/sms/phone", `{}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "tok-secret-123") {
		t.Errorf("the platform error leaked the token: %s", rec.Body.String())
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestSMSStatusAcceptsAPastedToken: POST is how the panel checks a token the
// operator pasted without restarting the process.
func TestSMSStatusAcceptsAPastedToken(t *testing.T) {
	c := &fakeSMSClient{fakeClient: &fakeClient{name: "wb"}, status: core.SMSStatus{Configured: true, Balance: "1.00"}}
	h := smsPanel(t, c)
	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/wb/sms", `{"token":"pasted"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["balance"] != "1.00" {
		t.Errorf("balance = %v", out["balance"])
	}
}
