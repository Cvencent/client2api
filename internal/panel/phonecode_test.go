package panel

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

type fakePhoneCodeClient struct {
	*fakeClient

	challenge core.PhoneCodeChallenge
	record    core.AccountRecord

	startPhone string
	finishID   string
	finishCode string
}

func (f *fakePhoneCodeClient) StartPhoneCodeLogin(_ context.Context, phone string) (core.PhoneCodeChallenge, error) {
	f.startPhone = phone
	return f.challenge, nil
}

func (f *fakePhoneCodeClient) FinishPhoneCodeLogin(_ context.Context, id, code string) (core.AccountRecord, error) {
	f.finishID = id
	f.finishCode = code
	return f.record, nil
}

func TestPhoneCodeRoutesPassValuesThroughAndReportTheCapability(t *testing.T) {
	c := &fakePhoneCodeClient{
		fakeClient: &fakeClient{name: "phone"},
		challenge:  core.PhoneCodeChallenge{ID: "challenge-1", ExpiresAt: "2026-10-12T12:00:00Z"},
		record:     core.AccountRecord{ID: "account-1", Label: "13900000001"},
	}
	h := loginPanel(t, c)

	caps := decodeMap(t, hitRoute(t, h, http.MethodGet, "/panel/api/clients/phone/capabilities", ""))
	if got, _ := caps["phone_code"].(bool); !got {
		t.Fatalf("capabilities did not report phone_code: %v", caps)
	}

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/phone/phone-code/start", `{"phone":"13900000001"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if c.startPhone != "13900000001" {
		t.Fatalf("start phone = %q, want the operator's number", c.startPhone)
	}
	if body := decodeMap(t, rec); body["id"] != "challenge-1" {
		t.Fatalf("start response = %v, want the challenge id", body)
	}

	rec = hitRoute(t, h, http.MethodPost, "/panel/api/clients/phone/phone-code/finish", `{"id":"challenge-1","code":"654321"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("finish status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if c.finishID != "challenge-1" || c.finishCode != "654321" {
		t.Fatalf("finish saw id=%q code=%q, want the submitted values", c.finishID, c.finishCode)
	}
	if body := decodeMap(t, rec); body["id"] != "account-1" {
		t.Fatalf("finish response = %v, want the stored account", body)
	}
}

func TestPhoneCodeRoutesValidateInputAndRefuseModulesWithoutTheCapability(t *testing.T) {
	c := &fakePhoneCodeClient{fakeClient: &fakeClient{name: "phone"}}
	h := loginPanel(t, c)

	if rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/phone/phone-code/start", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing phone status = %d, want 400", rec.Code)
	}
	if c.startPhone != "" {
		t.Fatal("a request with no phone reached the module")
	}
	if rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/phone/phone-code/finish", `{"id":"challenge-1"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing code status = %d, want 400", rec.Code)
	}
	if c.finishID != "" || c.finishCode != "" {
		t.Fatal("a request with no code reached the module")
	}

	plain := loginPanel(t, &fakeClient{name: "plain"})
	for _, path := range []string{
		"/panel/api/clients/plain/phone-code/start",
		"/panel/api/clients/plain/phone-code/finish",
	} {
		rec := hitRoute(t, plain, http.MethodPost, path, `{"phone":"13900000001","id":"challenge-1","code":"654321"}`)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s status = %d, want 501", path, rec.Code)
		}
	}
}

func TestPhoneCodeUIIsWiredToTheTwoStepRoutes(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`id="addPhoneCode"`,
		`id="phoneCodePhone"`,
		`id="btnPhoneCodeSend"`,
		`id="phoneCodeCode"`,
		`id="btnPhoneCodeFinish"`,
		`/phone-code/start`,
		`/phone-code/finish`,
		`data-mode="phone-code"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("phone-code UI is missing %s", want)
		}
	}

	finish := poolStatsFuncBody(t, src, "phoneCodeFinish")
	if !strings.Contains(finish, "await loginDone(") {
		t.Error("phoneCodeFinish does not share the normal login completion path")
	}

	relogin := poolStatsFuncBody(t, src, "openPhoneCodeRelogin")
	for _, want := range []string{`restore.phone = phone`, `$("#phoneCodePhone").value = phone`} {
		if !strings.Contains(relogin, want) {
			t.Errorf("openPhoneCodeRelogin is missing %s", want)
		}
	}
	if strings.Contains(relogin, "/phone-code/start") {
		t.Error("opening the phone-code re-login flow must not spend an SMS automatically")
	}
}
