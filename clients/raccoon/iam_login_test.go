package raccoon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- IAM envelope ---------------------------------------------------------

// The IAM service (which now serves the SMS login) answers with a symbolic
// `code` and its own message/request_id, not the web API's numeric envelope.
// Decoding that shape is what turns "captcha_token invalid" into something the
// operator can act on instead of a bare HTTP 400.
func TestDecodeEnvelopeReadsTheIAMShape(t *testing.T) {
	raw := []byte(`{"code":"COMMON_INVALID_ARGUMENT","message":"captcha_token invalid","request_id":"req-1","details":{}}`)

	_, err := decodeEnvelope(http.StatusBadRequest, raw, nil)
	if err == nil {
		t.Fatal("an IAM refusal must produce an error")
	}
	var ae *apiError
	if !asAPIError(err, &ae) {
		t.Fatalf("error %v is not an apiError", err)
	}
	if ae.IAMCode != "COMMON_INVALID_ARGUMENT" {
		t.Fatalf("IAMCode = %q, want COMMON_INVALID_ARGUMENT", ae.IAMCode)
	}
	if ae.RequestID != "req-1" {
		t.Fatalf("RequestID = %q, want req-1", ae.RequestID)
	}
	if !strings.Contains(err.Error(), "captcha_token invalid") {
		t.Fatalf("error = %q, want the IAM message", err.Error())
	}
}

// An IAM success carries `code: "OK"`; its `data` member is what callers read.
func TestDecodeEnvelopeAcceptsTheIAMSuccess(t *testing.T) {
	raw := []byte(`{"code":"OK","data":{"verification_id":"v-1","expires_in":300}}`)

	data, err := decodeEnvelope(http.StatusOK, raw, nil)
	if err != nil {
		t.Fatalf("OK envelope rejected: %v", err)
	}
	var got struct {
		VerificationID string `json:"verification_id"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("data is not the IAM payload: %v", err)
	}
	if got.VerificationID != "v-1" {
		t.Fatalf("verification_id = %q, want v-1", got.VerificationID)
	}
}

// The web API's numeric envelope must keep its meaning: captcha handling and
// the pool's auth checks all match on the numeric code.
func TestDecodeEnvelopeKeepsNumericCodesNumeric(t *testing.T) {
	raw := []byte(`{"code":100006,"message":"captcha_verify_error","details":"captcha verify failed"}`)

	_, err := decodeEnvelope(http.StatusBadRequest, raw, nil)
	if err == nil {
		t.Fatal("a 100006 refusal must produce an error")
	}
	var ae *apiError
	if !asAPIError(err, &ae) {
		t.Fatalf("error %v is not an apiError", err)
	}
	if ae.Code != 100006 {
		t.Fatalf("Code = %d, want 100006 (the numeric meaning must survive)", ae.Code)
	}
	if ae.IAMCode != "" {
		t.Fatalf("IAMCode = %q, want empty for a numeric envelope", ae.IAMCode)
	}
}

// A body that is not an envelope at all (an HTML 404 page, for instance) must
// still be reported as the HTTP failure it is.
func TestDecodeEnvelopeStillReportsPlainHTTPFailures(t *testing.T) {
	raw := []byte("404 page not found\n")

	_, err := decodeEnvelope(http.StatusNotFound, raw, nil)
	if err == nil {
		t.Fatal("a plain 404 must produce an error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %q, want it to name the status", err.Error())
	}
}

func asAPIError(err error, out **apiError) bool {
	for err != nil {
		if ae, ok := err.(*apiError); ok {
			*out = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// --- sendSMS / loginWithSMS wire contract ---------------------------------

// smsWireUpstream records the IAM login calls so the field names can be pinned.
type smsWireUpstream struct {
	t      *testing.T
	server *httptest.Server
	paths  []string
	bodies []map[string]any
	send   map[string]any
	login  map[string]any
}

func newSMSWireUpstream(t *testing.T) *smsWireUpstream {
	t.Helper()
	u := &smsWireUpstream{t: t}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		u.paths = append(u.paths, r.URL.Path)
		u.bodies = append(u.bodies, body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case pathIAMSendSMS:
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "OK", "data": map[string]any{
				"verification_id": "v-42", "expires_in": 300, "retry_after": 60,
			}})
		case pathIAMLoginSMS:
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "OK", "data": map[string]any{
				"access_token": "tok", "refresh_token": "ref", "token_type": "bearer",
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "404 page not found\n")
		}
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *smsWireUpstream) bodyFor(path string) map[string]any {
	for i := len(u.paths) - 1; i >= 0; i-- {
		if u.paths[i] == path {
			return u.bodies[i]
		}
	}
	return nil
}

// The send must go to IAM with the vendor's exact field names, and the
// challenge it returns is what the login step quotes.
func TestSendSMSTalksToTheIAMEndpoint(t *testing.T) {
	u := newSMSWireUpstream(t)
	c := newTestClient(t, t.TempDir(), `{"base_url":`+strconvQuote(u.server.URL)+`}`, nil)

	challenge, err := c.sendSMS(context.Background(), "13800001111", "slider-param")
	if err != nil {
		t.Fatalf("sendSMS: %v", err)
	}
	if challenge.VerificationID != "v-42" {
		t.Fatalf("verification_id = %q, want v-42", challenge.VerificationID)
	}

	sent := u.bodyFor(pathIAMSendSMS)
	if sent == nil {
		t.Fatal("the send did not reach the IAM endpoint")
	}
	if sent["captcha_token"] != "slider-param" {
		t.Fatalf("captcha_token = %v, want the slider parameter", sent["captcha_token"])
	}
	if sent["scene"] != smsLoginScene {
		t.Fatalf("scene = %v, want %q", sent["scene"], smsLoginScene)
	}
	if sent["nation_code"] != "86" {
		t.Fatalf("nation_code = %v, want 86", sent["nation_code"])
	}
	if _, leaked := sent["captcha_param"]; leaked {
		t.Fatal("the legacy captcha_param field must not be sent to IAM")
	}
}

func TestLoginWithSMSQuotesTheVerificationID(t *testing.T) {
	u := newSMSWireUpstream(t)
	c := newTestClient(t, t.TempDir(), `{"base_url":`+strconvQuote(u.server.URL)+`}`, nil)

	cred, err := c.loginWithSMS(context.Background(), "13800001111", "v-42", "654321")
	if err != nil {
		t.Fatalf("loginWithSMS: %v", err)
	}
	if cred.AccessToken != "tok" || cred.RefreshToken != "ref" {
		t.Fatalf("credential = %+v, want the IAM tokens", cred)
	}

	sent := u.bodyFor(pathIAMLoginSMS)
	if sent == nil {
		t.Fatal("the login did not reach the IAM endpoint")
	}
	if sent["verification_id"] != "v-42" {
		t.Fatalf("verification_id = %v, want v-42", sent["verification_id"])
	}
	if sent["verification_code"] != "654321" {
		t.Fatalf("verification_code = %v, want 654321", sent["verification_code"])
	}
	if _, leaked := sent["sms_code"]; leaked {
		t.Fatal("the legacy sms_code field must not be sent to IAM")
	}
}

// A missing verification_id must fail before any request goes out: the vendor
// answers such a call with an opaque invalid-argument error.
func TestLoginWithSMSRefusesWithoutAVerificationID(t *testing.T) {
	u := newSMSWireUpstream(t)
	c := newTestClient(t, t.TempDir(), `{"base_url":`+strconvQuote(u.server.URL)+`}`, nil)

	if _, err := c.loginWithSMS(context.Background(), "13800001111", "", "654321"); err == nil {
		t.Fatal("loginWithSMS accepted an empty verification_id")
	}
	if len(u.paths) != 0 {
		t.Fatalf("paths = %v, want no request before the id check", u.paths)
	}
}

// A captcha refusal must keep the operator-facing hint; anything else (a bad
// phone, a rate limit) must keep its own message.
func TestSMSSendErrorOnlyHintsAtTheCaptcha(t *testing.T) {
	captcha := &apiError{Status: 400, IAMCode: "COMMON_INVALID_ARGUMENT", Message: "captcha_token invalid"}
	if got := smsSendError(captcha).Error(); !strings.Contains(got, "滑块") {
		t.Fatalf("captcha refusal = %q, want the slider hint", got)
	}
	legacy := &apiError{Status: 400, Code: 100006, Message: "captcha_verify_error"}
	if got := smsSendError(legacy).Error(); !strings.Contains(got, "滑块") {
		t.Fatalf("legacy captcha refusal = %q, want the slider hint", got)
	}
	phone := &apiError{Status: 400, IAMCode: "COMMON_INVALID_ARGUMENT", Message: "phone number invalid"}
	if got := smsSendError(phone).Error(); strings.Contains(got, "滑块") {
		t.Fatalf("phone refusal = %q, must not be reported as a captcha problem", got)
	}
}
