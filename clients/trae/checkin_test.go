package trae

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// checkin_test.go drives the daily check-in entirely offline.  Every test uses
// a recording transport; nothing here reaches api.trae.cn.

// --- recorder ---------------------------------------------------------------

// rtRecorder captures what the module actually put on the wire.
type rtRecorder struct {
	mu      sync.Mutex
	urls    []string
	methods []string
	bodies  []string
	headers []http.Header
}

func (r *rtRecorder) record(req *http.Request) {
	var body string
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.urls = append(r.urls, req.URL.String())
	r.methods = append(r.methods, req.Method)
	r.bodies = append(r.bodies, body)
	r.headers = append(r.headers, req.Header.Clone())
}

func (r *rtRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.urls)
}

func (r *rtRecorder) snapshot() (urls []string, methods []string, bodies []string, headers []http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.urls...),
		append([]string(nil), r.methods...),
		append([]string(nil), r.bodies...),
		append([]http.Header(nil), r.headers...)
}

// checkinRT answers the status and claim endpoints with the given bodies.
func checkinRT(rec *rtRecorder, statusBody, claimBody string) roundTripFunc {
	return func(req *http.Request) (*http.Response, error) {
		rec.record(req)
		switch {
		case strings.HasSuffix(req.URL.Path, ugCheckinStatusPath):
			return jsonResponse(http.StatusOK, statusBody), nil
		case strings.HasSuffix(req.URL.Path, ugCheckinClaimPath):
			return jsonResponse(http.StatusOK, claimBody), nil
		default:
			return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"unexpected route"}`), nil
		}
	}
}

// --- actions ----------------------------------------------------------------

func TestTraeCheckinActionsOfferTheAction(t *testing.T) {
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, nil)
	got := c.CheckinActions(context.Background())
	if len(got) != 1 {
		t.Fatalf("got %d actions, want 1: %+v", len(got), got)
	}
	if got[0].ID != checkinActionUG {
		t.Errorf("action id = %q, want %q", got[0].ID, checkinActionUG)
	}
	if got[0].Label == "" || got[0].Help == "" {
		t.Errorf("action is missing its label or help: %+v", got[0])
	}
}

func TestTraeCheckinActionsWithoutAccounts(t *testing.T) {
	c := testClient(t, nil, nil, nil)
	if got := c.CheckinActions(context.Background()); len(got) != 0 {
		t.Fatalf("offered %+v with no accounts", got)
	}
}

func TestTraeCheckinActionsSkipForeignRegion(t *testing.T) {
	a := testAuth("u1", "TOK_1")
	a.Region = "US"
	c := testClient(t, nil, []*Auth{a}, nil)
	if got := c.CheckinActions(context.Background()); len(got) != 0 {
		t.Fatalf("offered %+v to a US account", got)
	}
}

func TestTraeCheckinCapabilitiesAdvertiseTheAction(t *testing.T) {
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, nil)
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Checkin {
		t.Fatal("caps.Checkin = false with an eligible account")
	}
	if !caps.CheckinReady {
		t.Fatal("caps.CheckinReady = false with an eligible account")
	}
	if len(caps.Actions) != 1 || caps.Actions[0].ID != checkinActionUG {
		t.Fatalf("caps.Actions = %+v", caps.Actions)
	}
}

func TestTraeCheckinCapabilitiesWithoutAccounts(t *testing.T) {
	c := testClient(t, nil, nil, nil)
	caps := core.CapabilitiesOf(context.Background(), c)
	if caps.CheckinReady {
		t.Fatalf("caps.CheckinReady = true with no accounts: %+v", caps.Actions)
	}
	// Trae still has check-in: the capability matrix asks the client-level
	// question and must keep answering yes.
	if !caps.Checkin {
		t.Fatal("caps.Checkin = false with no accounts: Trae implements CheckinProvider")
	}
}

// --- the happy paths --------------------------------------------------------

func TestTraeCheckinClaimsWhenNotCheckedIn(t *testing.T) {
	rec := &rtRecorder{}
	rt := checkinRT(rec, `{"checked_in":false,"enable":true,"credits":5}`,
		`{"code":0,"msg":"领取成功","credits":10}`)
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, rt)

	res, err := c.Checkin(context.Background(), "u1", checkinActionUG)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false, error=%q", res.Error)
	}
	if res.Action != checkinActionUG {
		t.Errorf("Action = %q", res.Action)
	}
	if res.AccountID != "u1" {
		t.Errorf("AccountID = %q", res.AccountID)
	}
	if res.Message != "领取成功" {
		t.Errorf("Message = %q", res.Message)
	}
	if got, _ := res.Data["credits"].(int64); got != 10 {
		t.Errorf("credits = %#v, want 10", res.Data["credits"])
	}
	if done, _ := res.Data["already_done"].(bool); done {
		t.Error("already_done = true for a fresh claim")
	}
	if res.At == "" {
		t.Error("At is empty")
	}

	urls, methods, bodies, _ := rec.snapshot()
	if len(urls) != 2 {
		t.Fatalf("made %d requests, want status then claim: %v", len(urls), urls)
	}
	if urls[0] != "https://api.trae.cn"+ugCheckinStatusPath {
		t.Errorf("first url = %q", urls[0])
	}
	if urls[1] != "https://api.trae.cn"+ugCheckinClaimPath {
		t.Errorf("second url = %q", urls[1])
	}
	for i, m := range methods {
		if m != http.MethodPost {
			t.Errorf("call %d used %s, want POST", i, m)
		}
	}
	for i, b := range bodies {
		if strings.TrimSpace(b) != "{}" {
			t.Errorf("call %d body = %q, want {}", i, b)
		}
	}
}

func TestTraeCheckinAlreadyCheckedInSkipsTheClaim(t *testing.T) {
	rec := &rtRecorder{}
	rt := checkinRT(rec, `{"checked_in":true,"enable":true,"credits":7}`, `{"credits":999}`)
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, rt)

	res, err := c.Checkin(context.Background(), "u1", "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false for an already-claimed day: %q", res.Error)
	}
	if done, _ := res.Data["already_done"].(bool); !done {
		t.Error("already_done = false, want true")
	}
	if got, _ := res.Data["credits"].(int64); got != 7 {
		t.Errorf("credits = %#v, want 7 from the status body", res.Data["credits"])
	}
	// The empty action must have resolved to the only action on offer.
	if res.Action != checkinActionUG {
		t.Errorf("Action = %q", res.Action)
	}
	if n := rec.count(); n != 1 {
		t.Fatalf("made %d requests; a claimed day must not be claimed again", n)
	}
}

func TestTraeCheckinToleratesTheWrappedShape(t *testing.T) {
	rec := &rtRecorder{}
	rt := checkinRT(rec, `{"code":0,"data":{"checked_in":false,"enable":true,"credits":3}}`,
		`{"code":0,"data":{"credits":9}}`)
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, rt)

	res, err := c.Checkin(context.Background(), "u1", checkinActionUG)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false when the state was wrapped in data: %q", res.Error)
	}
	if got, _ := res.Data["credits"].(int64); got != 9 {
		t.Errorf("credits = %#v, want 9 from the wrapped claim body", res.Data["credits"])
	}
}

func TestTraeCheckinWrappedAlreadyDone(t *testing.T) {
	rec := &rtRecorder{}
	rt := checkinRT(rec, `{"code":0,"data":{"checked_in":true,"enable":true,"credits":4}}`, `{}`)
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, rt)

	res, err := c.Checkin(context.Background(), "u1", checkinActionUG)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false: %q", res.Error)
	}
	if done, _ := res.Data["already_done"].(bool); !done {
		t.Error("already_done = false for a wrapped checked_in=true")
	}
	if n := rec.count(); n != 1 {
		t.Fatalf("made %d requests, want 1", n)
	}
}

// --- refusals ---------------------------------------------------------------

func TestTraeCheckinDisabledIsAResult(t *testing.T) {
	rec := &rtRecorder{}
	rt := checkinRT(rec, `{"checked_in":false,"enable":false,"msg":"not in the campaign"}`, `{}`)
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, rt)

	res, err := c.Checkin(context.Background(), "u1", checkinActionUG)
	if err != nil {
		t.Fatalf("a disabled campaign must not be a Go error, got %v", err)
	}
	if res.OK {
		t.Fatal("OK = true for a disabled campaign")
	}
	if !strings.Contains(res.Error, "not enabled") {
		t.Errorf("Error = %q", res.Error)
	}
	if !strings.Contains(res.Error, "not in the campaign") {
		t.Errorf("Error = %q, want the upstream text", res.Error)
	}
	if n := rec.count(); n != 1 {
		t.Fatalf("made %d requests, want only the status call", n)
	}
}

func TestTraeCheckinUpstreamFailureIsAResult(t *testing.T) {
	rec := &rtRecorder{}
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rec.record(req)
		return jsonResponse(http.StatusUnauthorized, `{"code":401,"msg":"token expired"}`), nil
	})
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, rt)

	res, err := c.Checkin(context.Background(), "u1", checkinActionUG)
	if err != nil {
		t.Fatalf("an upstream refusal must not be a Go error, got %v", err)
	}
	if res.OK {
		t.Fatal("OK = true for HTTP 401")
	}
	if res.Error == "" {
		t.Fatal("Error is empty")
	}
	if kind, _ := res.Data["error_kind"].(string); kind == "" {
		t.Errorf("error_kind missing from the result data: %#v", res.Data)
	}
}

func TestTraeCheckinForeignRegionIsAResult(t *testing.T) {
	rec := &rtRecorder{}
	rt := checkinRT(rec, `{"checked_in":false,"enable":true}`, `{}`)
	a := testAuth("u1", "TOK_1")
	a.Region = "US"
	c := testClient(t, nil, []*Auth{a}, rt)

	res, err := c.Checkin(context.Background(), "u1", checkinActionUG)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatal("OK = true for a non-CN account")
	}
	if !strings.Contains(res.Error, ugRegion) {
		t.Errorf("Error = %q, want it to name %s", res.Error, ugRegion)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("made %d requests for an ineligible account, want 0", n)
	}
}

func TestTraeCheckinUnknownActionIsAResult(t *testing.T) {
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, nil)
	res, err := c.Checkin(context.Background(), "u1", "no-such-action")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatal("OK = true for an unknown action")
	}
	if !strings.Contains(res.Error, "no-such-action") {
		t.Errorf("Error = %q", res.Error)
	}
}

func TestTraeCheckinUnknownAccountIsAnError(t *testing.T) {
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, nil)
	if _, err := c.Checkin(context.Background(), "nobody", checkinActionUG); err == nil {
		t.Fatal("want a Go error for an unknown account")
	}
}

func TestTraeCheckinParkedAccountIsAResult(t *testing.T) {
	rec := &rtRecorder{}
	c := panelClient(t, nil, checkinRT(rec, `{"checked_in":false,"enable":true}`, `{}`))
	added := addTestAccount(t, c, map[string]string{"access_token": "PANEL_TOKEN_0001"})

	if err := c.SetAccountEnabled(context.Background(), added.ID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	res, err := c.Checkin(context.Background(), added.ID, checkinActionUG)
	if err != nil {
		t.Fatalf("a parked account must not be a Go error, got %v", err)
	}
	if res.OK {
		t.Fatal("OK = true for a parked account")
	}
	if !strings.Contains(res.Error, "parked") {
		t.Errorf("Error = %q, want it to say the account is parked", res.Error)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("made %d requests for a parked account, want 0", n)
	}
}

// --- eligibility ------------------------------------------------------------

func TestTraeCheckinEligibility(t *testing.T) {
	tests := []struct {
		name string
		a    *Auth
		want bool
	}{
		{"nil", nil, false},
		{"region CN", &Auth{Region: "CN"}, true},
		{"region lowercase cn", &Auth{Region: "cn"}, true},
		{"region US", &Auth{Region: "US"}, false},
		{"no region, CN product", &Auth{Product: "Trae CN"}, true},
		{"no region, SOLO CN product", &Auth{Product: "TRAE SOLO CN"}, true},
		{"no region, international product", &Auth{Product: "Trae"}, false},
		{"no region, no product", &Auth{}, true},
		{"region wins over product", &Auth{Region: "US", Product: "Trae CN"}, false},
	}
	for _, tc := range tests {
		if got := checkinEligible(tc.a); got != tc.want {
			t.Errorf("%s: checkinEligible = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- wire shape -------------------------------------------------------------

func TestTraeCheckinRequestShape(t *testing.T) {
	rec := &rtRecorder{}
	rt := checkinRT(rec, `{"checked_in":false,"enable":true}`, `{"code":0,"credits":1}`)
	a := testAuth("u1", "TOK_1")
	a.DeviceID = "2235771921399404"
	c := testClient(t, nil, []*Auth{a}, rt)

	if _, err := c.Checkin(context.Background(), "u1", checkinActionUG); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	_, methods, _, headers := rec.snapshot()
	if len(headers) == 0 {
		t.Fatal("no request was recorded")
	}
	if methods[0] != http.MethodPost {
		t.Errorf("method = %q, want POST", methods[0])
	}
	h := headers[0]

	want := map[string]string{
		"Content-Type":  "application/json",
		"Accept":        "application/json",
		"User-Agent":    "Trae/" + c.cfg.ideVersion(),
		"Authorization": "Cloud-IDE-JWT TOK_1",
		"X-User-Region": ugRegion,
		"X-Device-Id":   "2235771921399404",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	// The check-in speaks the light ug identity, not the SOLO fingerprint the
	// chat path sends.  Leaking those would tie a billing call to a chat
	// session.
	for _, k := range []string{"X-App-Id", "X-Ide-Version", "X-Ide-Version-Code", "X-Cloudide-Token", "X-Ide-Token", "X-Uid", "X-Machine-Id", "X-Device-Type"} {
		if got := h.Get(k); got != "" {
			t.Errorf("%s = %q, want it absent from the check-in request", k, got)
		}
	}
}

// --- hygiene ----------------------------------------------------------------

func TestTraeCheckinNeverLeaksTheToken(t *testing.T) {
	const token = "SECRET_TRAE_TOKEN_0001"
	rec := &rtRecorder{}
	rt := checkinRT(rec, `{"checked_in":false,"enable":true}`, `{"code":0,"credits":1}`)
	a := testAuth("u1", token)
	c := testClient(t, nil, []*Auth{a}, rt)

	res, err := c.Checkin(context.Background(), "u1", checkinActionUG)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), token) {
		t.Fatalf("the result leaked the access token: %s", blob)
	}
}

func TestTraeCheckinTimeoutIsBounded(t *testing.T) {
	rec := &rtRecorder{}
	rt := checkinRT(rec, `{"checked_in":true,"enable":true,"credits":1}`, `{}`)
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, rt)

	start := time.Now()
	if _, err := c.Checkin(context.Background(), "u1", checkinActionUG); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if elapsed := time.Since(start); elapsed > checkinTimeout {
		t.Errorf("check-in took %s, longer than its own budget %s", elapsed, checkinTimeout)
	}
}
