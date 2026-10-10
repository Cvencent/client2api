package panel

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// fakeAutoClient opts a plain panel client into core.AutoLoginProvider and
// records the parameters each route handed it.
type fakeAutoClient struct {
	*fakeClient

	job       core.AutoLoginJob
	startErr  error
	pollErr   error
	cancelErr error

	started  []core.AutoLoginRequest
	polled   []string
	canceled []string
}

func (f *fakeAutoClient) StartAutoLogin(_ context.Context, req core.AutoLoginRequest) (core.AutoLoginJob, error) {
	f.started = append(f.started, req)
	if f.startErr != nil {
		return core.AutoLoginJob{}, f.startErr
	}
	job := f.job
	if job.ID == "" {
		job.ID = "run-1"
	}
	if job.State == "" {
		job.State = core.AutoLoginRunning
	}
	return job, nil
}

func (f *fakeAutoClient) PollAutoLogin(_ context.Context, id string) (core.AutoLoginJob, error) {
	f.polled = append(f.polled, id)
	if f.pollErr != nil {
		return core.AutoLoginJob{}, f.pollErr
	}
	job := f.job
	if job.ID == "" {
		job.ID = id
	}
	if job.State == "" {
		job.State = core.AutoLoginRunning
	}
	return job, nil
}

func (f *fakeAutoClient) CancelAutoLogin(_ context.Context, id string) error {
	f.canceled = append(f.canceled, id)
	return f.cancelErr
}

func autoPanel(t *testing.T, clients ...core.Client) http.Handler {
	t.Helper()
	return loginPanel(t, clients...)
}

// TestAutoLoginStartPassesTheRequestThrough: every field travels in the body
// and reaches the module untouched, and the accepted job comes back.
func TestAutoLoginStartPassesTheRequestThrough(t *testing.T) {
	c := &fakeAutoClient{fakeClient: &fakeClient{name: "wb"}}
	h := autoPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/wb/auto-login",
		`{"realm":"cn","token":"tok-1","proxy":"http://127.0.0.1:8080","keyword":"腾讯科技","province":"广东","card_type":"联通",`+
			`"phone":"17000000042","avoid":["17000000001"," 17000000002 ",""]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)
	if body["id"] != "run-1" || body["state"] != core.AutoLoginRunning {
		t.Errorf("start body = %v", body)
	}
	if len(c.started) != 1 {
		t.Fatalf("started = %d runs, want one", len(c.started))
	}
	got := c.started[0]
	if got.Realm != "cn" || got.Token != "tok-1" || got.Proxy != "http://127.0.0.1:8080" || got.Keyword != "腾讯科技" ||
		got.Province != "广东" || got.CardType != "联通" || got.Phone != "17000000042" {
		t.Errorf("request = %+v, want the body verbatim", got)
	}
	// The batch's avoid list travels through trimmed, so the module can merge it
	// with the pool without seeing empty or padded entries.
	if len(got.Avoid) != 2 || got.Avoid[0] != "17000000001" || got.Avoid[1] != "17000000002" {
		t.Errorf("avoid = %v, want [17000000001 17000000002]", got.Avoid)
	}

	// The capability matrix has to agree, or the panel would hide the button.
	caps := decodeMap(t, hitRoute(t, h, http.MethodGet, "/panel/api/clients/wb/capabilities", ""))
	if auto, _ := caps["auto_login"].(bool); !auto {
		t.Errorf("capabilities did not report auto_login: %v", caps)
	}
}

// TestAutoLoginPollAndCancel: GET reads the run without touching the network,
// DELETE stops it and reports the job's own state rather than assuming the
// cancel won the race.
func TestAutoLoginPollAndCancel(t *testing.T) {
	c := &fakeAutoClient{
		fakeClient: &fakeClient{name: "wb"},
		job: core.AutoLoginJob{
			ID:        "run-7",
			State:     core.AutoLoginSuccess,
			Message:   "登录成功",
			AccountID: "acc-9",
		},
	}
	h := autoPanel(t, c)

	rec := hitRoute(t, h, http.MethodGet, "/panel/api/clients/wb/auto-login/run-7", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("poll status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)
	if body["state"] != core.AutoLoginSuccess || body["account_id"] != "acc-9" {
		t.Errorf("poll body = %v", body)
	}
	if len(c.polled) != 1 || c.polled[0] != "run-7" {
		t.Errorf("polled = %v", c.polled)
	}

	del := hitRoute(t, h, http.MethodDelete, "/panel/api/clients/wb/auto-login/run-7", "")
	if del.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", del.Code, del.Body.String())
	}
	if len(c.canceled) != 1 || c.canceled[0] != "run-7" {
		t.Errorf("canceled = %v", c.canceled)
	}
}

// TestAutoLoginUnknownRunIs404: a poll for a run the module never accepted is
// a missing resource, not a silent empty job.
func TestAutoLoginUnknownRunIs404(t *testing.T) {
	c := &fakeAutoClient{fakeClient: &fakeClient{name: "wb"}, pollErr: errString("no such run")}
	h := autoPanel(t, c)

	rec := hitRoute(t, h, http.MethodGet, "/panel/api/clients/wb/auto-login/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestAutoLoginStartFailureIsRedacted: a failed start is a 502 whose text has
// been scrubbed of the platform credential.
func TestAutoLoginStartFailureIsRedacted(t *testing.T) {
	c := &fakeAutoClient{
		fakeClient: &fakeClient{name: "wb"},
		startErr:   errString("the SMS platform refused the request: token=tok-secret-123 is invalid"),
	}
	h := autoPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/wb/auto-login", `{}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "tok-secret-123") {
		t.Errorf("the start error leaked the token: %s", rec.Body.String())
	}
}

// TestAutoLoginJobIsRedacted: the log and the message are scrubbed on the way
// out even when the module stored a raw credential.
func TestAutoLoginJobIsRedacted(t *testing.T) {
	c := &fakeAutoClient{
		fakeClient: &fakeClient{name: "wb"},
		job: core.AutoLoginJob{
			ID:      "run-1",
			State:   core.AutoLoginFailed,
			Message: "rejected token=tok-secret-123",
			Log:     []core.AutoLogLine{{At: "t", Text: "using token=tok-secret-123"}},
		},
	}
	h := autoPanel(t, c)

	rec := hitRoute(t, h, http.MethodGet, "/panel/api/clients/wb/auto-login/run-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "tok-secret-123") {
		t.Errorf("the job leaked the token: %s", rec.Body.String())
	}
}

// TestAutoLoginRoutesAnswer501WithoutTheCapability: no button for a module that
// cannot drive its own login, and no silent success either.
func TestAutoLoginRoutesAnswer501WithoutTheCapability(t *testing.T) {
	h := autoPanel(t, &fakeClient{name: "plain"})
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/panel/api/clients/plain/auto-login", `{}`},
		{http.MethodGet, "/panel/api/clients/plain/auto-login/run-1", ""},
		{http.MethodDelete, "/panel/api/clients/plain/auto-login/run-1", ""},
	} {
		rec := hitRoute(t, h, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: status = %d, want 501 (body %s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// TestAutoLoginStartRejectsGet: the collection route only accepts POST.
func TestAutoLoginStartRejectsGet(t *testing.T) {
	c := &fakeAutoClient{fakeClient: &fakeClient{name: "wb"}}
	h := autoPanel(t, c)

	rec := hitRoute(t, h, http.MethodGet, "/panel/api/clients/wb/auto-login", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (body %s)", rec.Code, rec.Body.String())
	}
}
