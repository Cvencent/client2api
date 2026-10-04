package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// serverID is a well-formed server-issued request id (32 lowercase hex).
const serverID = "0123456789abcdef0123456789abcdef"

func TestWorkbuddyMarketExpertList(t *testing.T) {
	const data = `{"experts":[
	  {"expert_id":"ex_7f3a","expert_type":"agent","display_name_zh":"编程导师",
	   "profession_zh":"帮你写代码","version":"2.1.0","categories":["dev","code"]},
	  {"expert_id":"ex_9b1c","expert_type":"agent","display_name_zh":"文案助手"}
	]}`
	var seen *http.Request
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		seen = req
		return jsonResponse(200, wbEnvelope(data)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())
	a := wbCNAuth(t, c)

	got, err := c.MarketExpertList(context.Background(), a, "agent")
	if err != nil {
		t.Fatalf("MarketExpertList: %v", err)
	}
	req := wbIdentity(t, seen, a)
	if req.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", req.Method)
	}
	if want := "copilot.tencent.com" + expertListPath; req.URL.Host+req.URL.Path != want {
		t.Fatalf("url = %s%s, want %s", req.URL.Host, req.URL.Path, want)
	}
	body := wbBody(t, req)
	if body["page"] != float64(1) || body["page_size"] != float64(20) {
		t.Fatalf("page fields = %v/%v, want 1/20", body["page"], body["page_size"])
	}
	if body["sort_by"] != "reco_rank" || body["sort_order"] != "desc" {
		t.Fatalf("sort = %v/%v, want reco_rank/desc", body["sort_by"], body["sort_order"])
	}
	if body["expert_type"] != "agent" {
		t.Fatalf("expert_type = %v, want the caller's filter", body["expert_type"])
	}
	if req.Header.Get("User-Agent") != desktopUserAgent {
		t.Fatalf("User-Agent = %q, want the desktop UA", req.Header.Get("User-Agent"))
	}
	// The reference sends the full chat base as X-Domain, not just the host.
	if want := "https://copilot.tencent.com"; req.Header.Get("X-Domain") != want {
		t.Fatalf("X-Domain = %q, want the chat base %q", req.Header.Get("X-Domain"), want)
	}
	if req.Header.Get("X-Product") != "SaaS" {
		t.Fatalf("X-Product = %q, want SaaS", req.Header.Get("X-Product"))
	}

	if len(got) != 2 {
		t.Fatalf("got %d experts, want 2: %+v", len(got), got)
	}
	if got[0].ExpertID != "ex_7f3a" || got[0].DisplayNameZH != "编程导师" || got[0].ProfessionZH != "帮你写代码" {
		t.Fatalf("expert[0] = %+v, want the full record", got[0])
	}
	if got[0].Version != "2.1.0" || got[0].ExpertType != "agent" {
		t.Fatalf("expert[0] = %+v, want the version and type", got[0])
	}
	if n := expertCategory(got[0]); n != "dev" {
		t.Fatalf("expertCategory = %q, want the first category", n)
	}
	if n := expertCategory(got[1]); n != "expert-all" {
		t.Fatalf("expertCategory without categories = %q, want expert-all", n)
	}
	if v := expertVersion(got[1]); v != "1.0.0" {
		t.Fatalf("expertVersion without a version = %q, want 1.0.0", v)
	}
}

func TestWorkbuddyMarketExpertListOmitsTheTypeFilter(t *testing.T) {
	var seen *http.Request
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		seen = req
		return jsonResponse(200, wbEnvelope(`{"experts":[]}`)), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	if _, err := c.MarketExpertList(context.Background(), wbCNAuth(t, c), "  "); err != nil {
		t.Fatalf("MarketExpertList: %v", err)
	}
	if _, ok := wbBody(t, seen)["expert_type"]; ok {
		t.Fatal("an empty expert_type was still sent")
	}
}

func TestWorkbuddyMarketExpertListFailures(t *testing.T) {
	t.Run("business refusal", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(200, wbRefusal(11020, "the market is closed")), nil
		}}
		c, _ := panelClient(t, rt, cnAccountFiles())

		_, err := c.MarketExpertList(context.Background(), wbCNAuth(t, c), "")
		if err == nil || !strings.Contains(err.Error(), "11020") {
			t.Fatalf("error = %v, want the business code", err)
		}
	})

	t.Run("malformed payload", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(200, wbEnvelope(`[]`)), nil
		}}
		c, _ := panelClient(t, rt, cnAccountFiles())

		_, err := c.MarketExpertList(context.Background(), wbCNAuth(t, c), "")
		if err == nil || !strings.Contains(err.Error(), "expert list parse") {
			t.Fatalf("error = %v, want the parse step named", err)
		}
	})
}

// streamWithID builds one SSE frame carrying the server-issued request id.
func streamWithID(id string) string {
	return "data: {\"code\":0,\"msg\":\"\",\"data\":{\"id\":\"" + id +
		"\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"2\"}}]}}\n\n" +
		"data: [DONE]\n"
}

func TestWorkbuddyDesktopChatWithExpertCapturesTheServerRequestID(t *testing.T) {
	var seen *http.Request
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		seen = req
		return sseResponse(200, streamWithID(serverID), nil), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())
	a := wbCNAuth(t, c)

	conv, id, err := c.DesktopChatWithExpert(context.Background(), a, "ex_7f3a")
	if err != nil {
		t.Fatalf("DesktopChatWithExpert: %v", err)
	}
	if id != serverID {
		t.Fatalf("requestId = %q, want the server id %q", id, serverID)
	}
	if !strings.HasPrefix(conv, "wb2api-conv-") {
		t.Fatalf("conversationID = %q, want the wb2api-conv- prefix", conv)
	}

	if seen == nil {
		t.Fatal("no chat request was sent")
	}
	if want := "copilot.tencent.com" + chatCompletionsPath; seen.URL.Host+seen.URL.Path != want {
		t.Fatalf("url = %s%s, want %s", seen.URL.Host, seen.URL.Path, want)
	}
	if seen.Header.Get("X-Expert-Id") != "ex_7f3a" {
		t.Fatalf("X-Expert-Id = %q, want the expert", seen.Header.Get("X-Expert-Id"))
	}
	if seen.Header.Get("X-Conversation-ID") != conv {
		t.Fatalf("X-Conversation-ID = %q, want the returned conversation %q",
			seen.Header.Get("X-Conversation-ID"), conv)
	}
	if seen.Header.Get("x-codebuddy-request") != "1" {
		t.Fatal("x-codebuddy-request is missing")
	}
	if seen.Header.Get("X-Agent-Intent") != "craft" {
		t.Fatalf("X-Agent-Intent = %q, want craft", seen.Header.Get("X-Agent-Intent"))
	}
	if seen.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", seen.Header.Get("Accept"))
	}
	if got := seen.Header.Get("User-Agent"); got != desktopUserAgent {
		t.Fatalf("User-Agent = %q, want the desktop UA", got)
	}

	body := wbBody(t, seen)
	if body["model"] != "fast-model" {
		t.Fatalf("model = %v, want fast-model", body["model"])
	}
	if body["stream"] != true {
		t.Fatalf("stream = %v, want true", body["stream"])
	}
	if body["agent"] != "cli" {
		t.Fatalf("agent = %v, want cli", body["agent"])
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %v, want a system and a user message", body["messages"])
	}
	for i, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("message[%d] = %v", i, raw)
		}
		if m["role"] != "system" && m["role"] != "user" {
			t.Fatalf("message[%d].role = %v", i, m["role"])
		}
		if s, _ := m["content"].(string); s == "" {
			t.Fatalf("message[%d] has no content", i)
		}
	}
}

// A locally invented id in the stream must be skipped: only a server-shaped id
// counts, because only that one will be scored.
func TestWorkbuddyDesktopChatWithExpertSkipsALocallyInventedID(t *testing.T) {
	stream := "data: {\"code\":0,\"data\":{\"id\":\"wb2api-conv-1\",\"choices\":[]}}\n\n" +
		streamWithID(serverID)
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return sseResponse(200, stream, nil), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	_, id, err := c.DesktopChatWithExpert(context.Background(), wbCNAuth(t, c), "ex_1")
	if err != nil {
		t.Fatalf("DesktopChatWithExpert: %v", err)
	}
	if id != serverID {
		t.Fatalf("requestId = %q, want the server id, not the local one", id)
	}
}

func TestWorkbuddyDesktopChatWithExpertConflictCases(t *testing.T) {
	t.Run("a stream without an id", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			return sseResponse(200, "data: {\"code\":0,\"data\":{\"id\":\"nope\"}}\n\ndata: [DONE]\n", nil), nil
		}}
		c, _ := panelClient(t, rt, cnAccountFiles())

		conv, id, err := c.DesktopChatWithExpert(context.Background(), wbCNAuth(t, c), "ex_1")
		if err == nil {
			t.Fatalf("returned conv=%q id=%q and no error", conv, id)
		}
		if !strings.Contains(err.Error(), "no server-issued requestId") {
			t.Fatalf("error = %q, want it to explain the missing id", err)
		}
		if id != "" {
			t.Fatalf("id = %q, want empty on failure", id)
		}
	})

	t.Run("an http error", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusBadGateway, `{"code":6004,"msg":"rate limited"}`), nil
		}}
		c, _ := panelClient(t, rt, cnAccountFiles())

		_, _, err := c.DesktopChatWithExpert(context.Background(), wbCNAuth(t, c), "ex_1")
		if err == nil || !strings.Contains(err.Error(), "chat http 502") {
			t.Fatalf("error = %v, want it to name the status", err)
		}
	})
}

func TestWorkbuddyScanServerRequestID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain", `{"id":"` + serverID + `"}`, serverID, true},
		{"cmb prefix", `{"id":"cmb-` + serverID + `"}`, "cmb-" + serverID, true},
		{"uppercase hex is not a server id", `{"id":"0123456789ABCDEF0123456789ABCDEF"}`, "", false},
		{"too short", `{"id":"0123456789abcdef"}`, "", false},
		{"no id at all", `{"choices":[]}`, "", false},
		{
			"the first valid id wins",
			`{"id":"local-1","choices":[],"other":"` + serverID + `","id":"` + serverID + `"}`,
			serverID, true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := scanServerRequestID([]byte(tc.in))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("scanServerRequestID(%s) = %q/%v, want %q/%v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestWorkbuddyDesktopExpertSummonSequence(t *testing.T) {
	e := MarketExpert{
		ExpertID:      "ex_7f3a",
		ExpertType:    "agent",
		DisplayNameZH: "编程导师",
		ProfessionZH:  "帮你写代码",
		Version:       "2.1.0",
		Categories:    []any{"dev"},
	}
	got := DesktopExpertSummonSequence(e)
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	if got[0]["eventCode"] != "web_element_click" || got[1]["eventCode"] != "expert_summon_click" ||
		got[2]["eventCode"] != "expert_summoned" {
		t.Fatalf("event codes = %v/%v/%v",
			got[0]["eventCode"], got[1]["eventCode"], got[2]["eventCode"])
	}
	if got[0]["elementId"] != "expert_summon_click" || got[0]["source"] != "ex_7f3a" {
		t.Fatalf("click[0] = %v, want the expert card click", got[0])
	}
	if name, _ := got[0]["elementName"].(string); name == "" {
		t.Fatalf("click[0].elementName is empty: %v", got[0])
	}
	if got[1]["mode"] != "LOCAL" {
		t.Fatalf("summon mode = %v, want LOCAL", got[1]["mode"])
	}
	for i, ev := range got[1:] {
		if ev["id"] != "ex_7f3a" || ev["name"] != "编程导师" || ev["expertTitle"] != "帮你写代码" {
			t.Fatalf("event[%d] = %v, want the expert identity", i+1, ev)
		}
	}
	if got[1]["version"] != "2.1.0" || got[1]["type"] != "expert-all" {
		t.Fatalf("summon = %v, want the version and catch-all bucket", got[1])
	}
}

func TestWorkbuddyDesktopExpertActualUseEvents(t *testing.T) {
	e := MarketExpert{ExpertID: "ex_7f3a", ExpertType: "agent", DisplayNameZH: "编程导师", ProfessionZH: "帮你写代码"}
	craft := DesktopExpertActualUseEvent(e, "wb2api-conv-1", serverID)
	local := DesktopExpertActualUseLocal(e, "wb2api-conv-1", serverID)

	if craft["mode"] != "craft" {
		t.Fatalf("craft mode = %v, want craft", craft["mode"])
	}
	if local["mode"] != "LOCAL" {
		t.Fatalf("local mode = %v, want LOCAL", local["mode"])
	}
	want := map[string]any{
		"eventCode":      "expert_actual_use",
		"id":             "ex_7f3a",
		"name":           "编程导师",
		"expertTitle":    "帮你写代码",
		"type":           "expert-all",
		"expertType":     "agent",
		"source":         "builtin",
		"version":        "1.0.0",
		"cost":           9000,
		"characterCount": 14,
		"conversationId": "wb2api-conv-1",
		"requestId":      serverID,
		"messageId":      "msg-" + serverID[len(serverID)-8:],
	}
	for k, w := range want {
		if craft[k] != w {
			t.Fatalf("craft[%q] = %v, want %v", k, craft[k], w)
		}
		if local[k] != w {
			t.Fatalf("local[%q] = %v, want %v", k, local[k], w)
		}
	}
	if len(craft) != len(want)+1 || len(local) != len(want)+1 {
		t.Fatalf("event key counts = %d/%d, want %d plus mode", len(craft), len(local), len(want))
	}
}

func TestWorkbuddyIDRegex(t *testing.T) {
	for _, ok := range []string{serverID, "cmb-" + serverID} {
		if !idRegex.MatchString(ok) {
			t.Errorf("idRegex rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "cmb-", "wb2api-conv-1", serverID + "0", strings.ToUpper(serverID)} {
		if idRegex.MatchString(bad) {
			t.Errorf("idRegex accepted %q", bad)
		}
	}
	raw, err := json.Marshal(DesktopExpertActualUseLocal(MarketExpert{}, "", ""))
	if err != nil {
		t.Fatalf("the event must be JSON-serialisable: %v", err)
	}
	if !strings.Contains(string(raw), `"cost":9000`) {
		t.Fatalf("serialised event = %s, want the numeric cost", raw)
	}
}
