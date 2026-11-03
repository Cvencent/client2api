package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 对话记录的接口层：这是上一组最脆弱的地方，单独再钉几件事。
//   1. 创建/追加的消息文本、思考内容、诊断信息都要原样往返；
//   2. 一次 PATCH 能把整个会话（标题 + 上下文 + 模型/账号）落盘，页面保存
//      对话时就是这么调的；
//   3. 打开对话再追加，历史不丢，多轮上下文按顺序保留。
// ---------------------------------------------------------------------------

func TestChatHistoryHTTPPreservesTurnFields(t *testing.T) {
	h := New(Options{DataDir: t.TempDir()})

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/chat/conversations", `{"title":"t","model":"m"}`)
	var t1 ChatThread
	if err := json.Unmarshal(rec.Body.Bytes(), &t1); err != nil {
		t.Fatalf("decode: %v", err)
	}

	body := `{"messages":[
		{"role":"assistant","content":"正文","reasoning":"一段思考","meta":"HTTP 200 · 42 ms"}]}`
	rec = hitRoute(t, h, http.MethodPatch, "/panel/api/chat/conversations/"+t1.ID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, body %s", rec.Code, rec.Body.String())
	}
	var got ChatThread
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	m := got.Messages[0]
	if m.Content != "正文" || m.Reasoning != "一段思考" || m.Meta != "HTTP 200 · 42 ms" {
		t.Fatalf("turn round trip = %+v", m)
	}
}

func TestChatHistoryHTTPKeepsOrderAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	h := New(Options{DataDir: dir})

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/chat/conversations", `{"title":"多轮"}`)
	var t1 ChatThread
	if err := json.Unmarshal(rec.Body.Bytes(), &t1); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, step := range []string{
		`{"messages":[{"role":"user","content":"第一问"}]}`,
	} {
		if rec := hitRoute(t, h, http.MethodPatch, "/panel/api/chat/conversations/"+t1.ID, step); rec.Code != http.StatusOK {
			t.Fatalf("patch = %d, body %s", rec.Code, rec.Body.String())
		}
	}
	// 第二轮：先读回来，再追加，模拟页面打开旧对话继续问。
	rec = hitRoute(t, h, http.MethodGet, "/panel/api/chat/conversations/"+t1.ID, "")
	var reopened ChatThread
	if err := json.Unmarshal(rec.Body.Bytes(), &reopened); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	reopened.Messages = append(reopened.Messages,
		ChatTurn{Role: "assistant", Content: "第一答"},
		ChatTurn{Role: "user", Content: "第二问"},
	)
	raw, _ := json.Marshal(map[string]any{"messages": reopened.Messages})
	if rec := hitRoute(t, h, http.MethodPatch, "/panel/api/chat/conversations/"+t1.ID, string(raw)); rec.Code != http.StatusOK {
		t.Fatalf("second patch = %d, body %s", rec.Code, rec.Body.String())
	}

	// 换一个进程（同一个 data 目录）读，顺序必须保持。
	//
	// 列表只回摘要（issue #127），所以正文从**单条对话**端点读——这也正是页面
	// 打开一条对话时走的路径，测试跟着它才算测到真实读取链。
	again := New(Options{DataDir: dir})
	rec = hitRoute(t, again, http.MethodGet, "/panel/api/chat/conversations/"+t1.ID, "")
	want := []string{"第一问", "第一答", "第二问"}
	for i, w := range want {
		if !strings.Contains(rec.Body.String(), w) {
			t.Fatalf("重启后丢了第 %d 条 %q: %s", i+1, w, rec.Body.String())
		}
	}
	var thread ChatThread
	if err := json.Unmarshal(rec.Body.Bytes(), &thread); err != nil {
		t.Fatalf("decode thread: %v", err)
	}
	got := thread.Messages
	if len(got) != 3 || got[0].Content != "第一问" || got[2].Content != "第二问" {
		t.Fatalf("order = %+v", got)
	}

	// 列表本身：这一条必须出现，但**不带正文**，只报条数（issue #127）。
	rec = hitRoute(t, again, http.MethodGet, "/panel/api/chat/conversations", "")
	if strings.Contains(rec.Body.String(), "第一问") {
		t.Fatalf("列表仍然回传消息正文：整库体积的问题没有解决（issue #127）:\n%s", rec.Body.String())
	}
	var list struct {
		Conversations []ChatThreadSummary `json:"conversations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Conversations) != 1 || list.Conversations[0].MessageCount != 3 {
		t.Fatalf("摘要列表 = %+v，期望 1 条、message_count=3", list.Conversations)
	}
}

func TestChatHistoryHTTPBodyIsRenderedAsArray(t *testing.T) {
	h := New(Options{DataDir: t.TempDir()})
	rec := hitRoute(t, h, http.MethodGet, "/panel/api/chat/conversations", "")
	if !strings.Contains(rec.Body.String(), `"conversations":[]`) {
		t.Fatalf("empty history must serialize as [], got %s", rec.Body.String())
	}
}
