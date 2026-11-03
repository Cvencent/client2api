package gateway

import (
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// 不认识的内容块必须 400 并点名类型（issue #154）。
//
// 三个入口的 content 翻译层原本对不认识的块静默跳过：请求带着剩下的（可能
// 为空的）提示发给上游、照常回 200——发音频的客户端拿到的是模型对空提示的
// 一句泛泛回复，用量页记成成功，操作员毫无线索。与 #41 的 droppedParams
// 相反：参数丢了上游还能答，块丢了答案是错的。
//
// 判据是三条协议各自的拒绝路径都带块类型（操作员能对着报错改请求），并且
// 上游一次都没被碰到。上游消费者合法的类型（Anthropic 的 thinking/tool_use/
// tool_result、agent 信封的 encrypted_content、Responses 的 input_text）不在
// 拒绝之列——最后一组测试专门钉这个，防止白名单收紧过头。
// ---------------------------------------------------------------------------

func harness154(t *testing.T) (*testClient, *http.Server) {
	t.Helper()
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	return c, newTestServer(t, c, NewStats(), NewUsageStore(10))
}

// sawUpstream reports whether the fake module observed a chat request.
func sawUpstream(c *testClient) bool { return c.seen != nil }

// TestChatContentWithAudioBlockIsRejectedWithTheType 钉 Chat 入口。
func TestChatContentWithAudioBlockIsRejectedWithTheType(t *testing.T) {
	c, srv := harness154(t)
	body := `{"model":"t/m1","messages":[{"role":"user","content":[
		{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`
	rec := chat(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("audio-only content = %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "input_audio") {
		t.Errorf("400 没有点名块类型 input_audio：%s（操作员无从知道是块被丢，issue #154）", rec.Body)
	}
	if sawUpstream(c) {
		t.Error("拒绝之前请求已经到了上游：静默空跑正是本单要堵的形状（issue #154）")
	}

	// 混合块：文本保留、音频整条拒绝——不能只丢块然后放行。
	rec = chat(t, srv, `{"model":"t/m1","messages":[{"role":"user","content":[
		{"type":"text","text":"这段录音说了什么"},
		{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("text+audio content = %d, want 400：丢块放行会让上游只看到半条提示（issue #154）", rec.Code)
	}
	if sawUpstream(c) {
		t.Error("混合块的请求到了上游（issue #154）")
	}

	// 对照：纯文本照常通过。
	rec = chat(t, srv, `{"model":"t/m1","messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("plain text content = %d, want 200：%s", rec.Code, rec.Body)
	}
	if !sawUpstream(c) {
		t.Fatal("纯文本请求没有到达上游：误伤了正常路径")
	}
}

// TestResponsesContentWithFileBlockIsRejectedWithTheType 钉 Responses 入口
// （input_file 在 parseResponsesContent 的重写 switch 之外，重写后原本还要被
// 共享解码器丢第二次）。
func TestResponsesContentWithFileBlockIsRejectedWithTheType(t *testing.T) {
	c, srv := harness154(t)
	body := `{"model":"t/m1","input":[{"type":"message","role":"user","content":[
		{"type":"input_file","filename":"spec.pdf","file_data":"data:application/pdf;base64,AAAA"}]}]}`
	rec := responses(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("input_file content = %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "input_file") {
		t.Errorf("400 没有点名块类型 input_file：%s（issue #154）", rec.Body)
	}
	if sawUpstream(c) {
		t.Error("拒绝之前请求已经到了上游（issue #154）")
	}
}

// TestAnthropicDocumentBlockIsRejectedWithTheType 钉 Anthropic 入口，并确认
// 报错不再是那条误导性的 "messages must not be empty"——块类型不认识要说的是
// 「块我不认识」，不是「你没发消息」。
func TestAnthropicDocumentBlockIsRejectedWithTheType(t *testing.T) {
	c, srv := harness154(t)
	body := `{"model":"t/m1","max_tokens":8,"messages":[{"role":"user","content":[
		{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AAAA"}}]}]}`
	rec := messages(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("document-only content = %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "document") {
		t.Errorf("400 没有点名块类型 document：%s（issue #154）", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "messages must not be empty") {
		t.Errorf("报错仍是误导性的「messages must not be empty」：%s（病因是块类型，不是空消息，issue #154）", rec.Body)
	}
	if sawUpstream(c) {
		t.Error("拒绝之前请求已经到了上游（issue #154）")
	}
}

// TestLegalContentPartsStillPass 确认上游消费者合法的类型没有被误伤：
// Anthropic 的 thinking/tool_use/tool_result、agent 信封的 encrypted_content、
// Responses 的 input_text。
func TestLegalContentPartsStillPass(t *testing.T) {
	c, srv := harness154(t)

	// Anthropic：assistant 带 thinking + tool_use，user 带 tool_result。
	rec := messages(t, srv, `{"model":"t/m1","max_tokens":64,"messages":[
		{"role":"user","content":"run the build"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"plan","signature":"sig"},
			{"type":"tool_use","id":"call1","name":"exec_command","input":{"cmd":"go test"}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"call1","content":"ok"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("thinking/tool_use/tool_result = %d, want 200: %s（issue #154 的误伤检查）", rec.Code, rec.Body)
	}
	if !sawUpstream(c) {
		t.Fatal("合法块组合没有到达上游")
	}

	// Responses：agent 信封带 encrypted_content——密文按既有契约被替换成
	// 「[encrypted agent payload omitted]」，信封文本保留，而不是拒绝整条。
	c2, srv2 := harness154(t)
	rec = responses(t, srv2, `{"model":"t/m1","input":[
		{"type":"agent_message","id":"amsg_1","author":"/root","recipient":"/root/review",
		 "content":[
			{"type":"input_text","text":"Message Type: NEW_TASK"},
			{"type":"encrypted_content","encrypted_content":"AAAA"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("agent envelope with encrypted_content = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !sawUpstream(c2) {
		t.Fatal("agent 信封没有到达上游：encrypted_content 被误伤（issue #154）")
	}
	var envelope string
	for _, m := range c2.seen.Messages {
		if strings.Contains(m.Content, "NEW_TASK") {
			envelope = m.Content
		}
	}
	if envelope == "" {
		t.Fatal("信封文本丢失（issue #154 的误伤检查）")
	}
	if strings.Contains(envelope, "AAAA") {
		t.Errorf("密文被转发给模型：%q", envelope)
	}
}
