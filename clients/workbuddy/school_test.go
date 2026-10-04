package workbuddy

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestWorkbuddySchoolVouchersListsTheRedemptionCodes(t *testing.T) {
	const data = `{"items":[
	  {"grant_id":7,"draw_uuid":"d-7","sku_code":"sku-7","prize_name":"会员月卡","code":"WB-AAAA-1111",
	   "valid_from":"2026-01-01","valid_to":"2026-02-01","granted_at":"2026-01-02T03:04:05Z"},
	  {"grant_id":8,"prize_name":"积分","code":"WB-BBBB-2222"}
	]}`
	var seen *http.Request
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		seen = req
		return jsonResponse(200, wbEnvelope(data)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())
	a := wbCNAuth(t, c)

	got, err := c.SchoolVouchers(context.Background(), a)
	if err != nil {
		t.Fatalf("SchoolVouchers: %v", err)
	}
	req := wbIdentity(t, seen, a)
	if req.Method != http.MethodGet {
		t.Fatalf("method = %s, want GET", req.Method)
	}
	if want := schoolBase + "/vouchers"; req.URL.Path != want {
		t.Fatalf("path = %s, want %s", req.URL.Path, want)
	}
	if req.URL.Host != "www.codebuddy.cn" {
		t.Fatalf("host = %s, want the CN billing base", req.URL.Host)
	}
	if len(got) != 2 {
		t.Fatalf("got %d vouchers, want 2: %+v", len(got), got)
	}
	if got[0].Code != "WB-AAAA-1111" || got[0].PrizeName != "会员月卡" || got[0].GrantID != 7 {
		t.Fatalf("voucher[0] = %+v, want the full record", got[0])
	}
	if got[0].ValidTo != "2026-02-01" || got[0].DrawUUID != "d-7" || got[0].SKUCode != "sku-7" {
		t.Fatalf("voucher[0] = %+v, want the validity and draw fields", got[0])
	}
	if got[1].Code != "WB-BBBB-2222" {
		t.Fatalf("voucher[1].Code = %q, want the second code", got[1].Code)
	}
}

// An empty voucher list is an empty slice and no error, not a failure.
func TestWorkbuddySchoolVouchersWithoutPrizes(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, wbEnvelope(`{"items":[]}`)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	got, err := c.SchoolVouchers(context.Background(), wbCNAuth(t, c))
	if err != nil {
		t.Fatalf("SchoolVouchers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d vouchers, want none", len(got))
	}
}

func TestWorkbuddySchoolVouchersSurfacesARefusal(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, wbRefusal(11010, "the activity is over")), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	_, err := c.SchoolVouchers(context.Background(), wbCNAuth(t, c))
	if err == nil {
		t.Fatal("a refused voucher list returned no error")
	}
	if !strings.Contains(err.Error(), "11010") {
		t.Fatalf("error = %q, want it to name the business code", err)
	}
}

func TestWorkbuddyMiniExpertUseEvent(t *testing.T) {
	ev := MiniExpertUseEvent("ex_abc123", "编程导师", "")
	want := map[string]any{
		"eventCode":      "expert_actual_use",
		"reportDelay":    0,
		"extVersion":     "2.2.8",
		"source":         "mini_program",
		"id":             "ex_abc123",
		"name":           "ex_abc123",
		"expertTitle":    "编程导师",
		"type":           "send_message",
		"characterCount": 12,
		"expertType":     "agent",
	}
	if len(ev) != len(want) {
		t.Fatalf("event has %d keys, want %d: %v", len(ev), len(want), ev)
	}
	for k, w := range want {
		if ev[k] != w {
			t.Fatalf("event[%q] = %v, want %v", k, ev[k], w)
		}
	}
	// The standalone school event must not pretend to belong to a conversation.
	for _, banned := range []string{"conversationId", "activityId"} {
		if _, ok := ev[banned]; ok {
			t.Fatalf("event carries %q, which the school scoring rejects", banned)
		}
	}
}

func TestWorkbuddyMiniExpertUseEventDefaults(t *testing.T) {
	ev := MiniExpertUseEvent("ex_only_id", "", "custom")
	if ev["expertTitle"] != "ex_only_id" {
		t.Fatalf("expertTitle = %v, want the id when no name is given", ev["expertTitle"])
	}
	if ev["expertType"] != "custom" {
		t.Fatalf("expertType = %v, want the caller's type", ev["expertType"])
	}
}

func TestWorkbuddyMiniChatModelEventCarriesTheModel(t *testing.T) {
	ev := MiniChatModelEvent("wb2api-mp-conv-1", "glm-5.2", "GLM-5.2")
	if ev["eventCode"] != "chat_request_send" {
		t.Fatalf("eventCode = %v, want chat_request_send", ev["eventCode"])
	}
	if ev["requestModelId"] != "glm-5.2" || ev["requestModelName"] != "GLM-5.2" {
		t.Fatalf("model fields = %v/%v, want glm-5.2/GLM-5.2",
			ev["requestModelId"], ev["requestModelName"])
	}
	if ev["conversationId"] != "wb2api-mp-conv-1" {
		t.Fatalf("conversationId = %v, want the caller's conversation", ev["conversationId"])
	}
	if ev["activityId"] != mpSchoolActivityID {
		t.Fatalf("activityId = %v, want %s", ev["activityId"], mpSchoolActivityID)
	}
	if ev["traceId"] == "" || ev["messageId"] == "" {
		t.Fatalf("the chat fingerprint is incomplete: %v", ev)
	}
}

func TestWorkbuddyMiniChatModelEventDefaultsTheName(t *testing.T) {
	ev := MiniChatModelEvent("conv", "glm-5.2", "")
	if ev["requestModelName"] != "glm-5.2" {
		t.Fatalf("requestModelName = %v, want the id", ev["requestModelName"])
	}
}

func TestWorkbuddyMiniPlaybookEvents(t *testing.T) {
	got := MiniPlaybookEvents("pm-gtm-launch-plan", "一页纸")
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	cta, send := got[0], got[1]

	for i, ev := range got {
		want := map[string]any{
			"id":           "pm-gtm-launch-plan",
			"name":         "一页纸",
			"type":         "document",
			"categoryId":   "",
			"categoryName": "",
			"skills":       "",
			"skillNames":   "",
		}
		for k, w := range want {
			if ev[k] != w {
				t.Fatalf("event[%d][%q] = %v, want %v", i, k, ev[k], w)
			}
		}
		if ev["extVersion"] != "2.2.8" {
			t.Fatalf("event[%d].extVersion = %v, want 2.2.8", i, ev["extVersion"])
		}
	}

	if cta["eventCode"] != "playbook_cta_click" || cta["source"] != "discover" || cta["position"] != 1 {
		t.Fatalf("cta = %v, want the click shape", cta)
	}
	if send["eventCode"] != "playbook_prompt_send" || send["source"] != "discover" {
		t.Fatalf("send = %v, want the prompt shape", send)
	}
	if send["promptLength"] != 96 || send["isOfficial"] != 1 {
		t.Fatalf("send = %v, want promptLength 96 and isOfficial 1", send)
	}
	conv, _ := send["conversationId"].(string)
	if !strings.HasPrefix(conv, "wb2api-mp-pb-") || conv == "wb2api-mp-pb-" {
		t.Fatalf("send.conversationId = %q, want the wb2api-mp-pb- prefix and a token", conv)
	}
	// The two events must not alias each other's keys.
	cta["mutated"] = true
	if _, leaked := send["mutated"]; leaked {
		t.Fatal("the two playbook events share one map")
	}
}
