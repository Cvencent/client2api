package panel

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 对话测试的记录：跟着 data 目录走。
//
// 这一组先钉服务端的持久化契约，再钉 HTTP 面。前面每条都是先写测试、先看它
// 失败，然后才补实现：
//   1. 记录落在 <data>/panel/chat_history.json，进程重启后还在；
//   2. 新建/改名/追加消息/删除单条/清空都能通过 HTTP 完成；
//   3. 列表按最近更新排序，且返回完整消息（前端点开历史不用第二次请求）；
//   4. 没有 data 目录时退化成内存存储，而不是把请求打崩。
// ---------------------------------------------------------------------------

func TestChatHistoryStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel", "chat_history.json")

	store := newChatHistoryStore(path)
	thread, err := store.Create(ChatThread{Title: "第一段对话", Client: "trae", Model: "trae/deepseek"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if thread.ID == "" {
		t.Fatal("Create returned no id")
	}
	if _, err := store.Append(thread.ID, ChatTurn{Role: "user", Content: "你好"}); err != nil {
		t.Fatalf("Append user: %v", err)
	}
	if _, err := store.Append(thread.ID, ChatTurn{Role: "assistant", Content: "你好，有什么可以帮你？"}); err != nil {
		t.Fatalf("Append assistant: %v", err)
	}

	reopened := newChatHistoryStore(path)
	got, ok := reopened.Get(thread.ID)
	if !ok {
		t.Fatalf("thread %s did not survive reopen", thread.ID)
	}
	if got.Title != "第一段对话" || len(got.Messages) != 2 {
		t.Fatalf("reopened = %+v", got)
	}
	if got.Messages[1].Content != "你好，有什么可以帮你？" {
		t.Fatalf("assistant content = %q", got.Messages[1].Content)
	}
}

func TestChatHistoryStoreListNewestFirst(t *testing.T) {
	store := newChatHistoryStore("")
	a, _ := store.Create(ChatThread{Title: "A"})
	b, _ := store.Create(ChatThread{Title: "B"})
	if _, err := store.Append(a.ID, ChatTurn{Role: "user", Content: "touch A"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	list := store.List()
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	if list[0].ID != a.ID {
		t.Fatalf("newest = %s, want the just-updated %s", list[0].ID, a.ID)
	}
	if list[1].ID != b.ID {
		t.Fatalf("second = %s, want %s", list[1].ID, b.ID)
	}
}

func TestChatHistoryStoreDeleteAndClear(t *testing.T) {
	store := newChatHistoryStore("")
	a, _ := store.Create(ChatThread{Title: "A"})
	b, _ := store.Create(ChatThread{Title: "B"})

	if !store.Delete(a.ID) {
		t.Fatal("Delete reported nothing removed")
	}
	if _, ok := store.Get(a.ID); ok {
		t.Fatal("deleted thread is still readable")
	}
	if store.Delete(a.ID) {
		t.Fatal("second Delete reported a removal")
	}
	if got := store.List(); len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("after delete = %+v", got)
	}
	store.Clear()
	if got := store.List(); len(got) != 0 {
		t.Fatalf("after clear = %+v", got)
	}
}

func TestChatHistoryStoreRename(t *testing.T) {
	store := newChatHistoryStore("")
	a, _ := store.Create(ChatThread{Title: "旧名字"})
	got, err := store.Rename(a.ID, "  新名字  ")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got.Title != "新名字" {
		t.Fatalf("title = %q, want trimmed 新名字", got.Title)
	}
	if _, err := store.Rename("nope", "x"); err == nil {
		t.Fatal("Rename of an unknown id succeeded")
	}
}

func TestChatHistoryStoreRejectsUnknownAppend(t *testing.T) {
	store := newChatHistoryStore("")
	if _, err := store.Append("missing", ChatTurn{Role: "user", Content: "hi"}); err == nil {
		t.Fatal("Append to an unknown thread succeeded")
	}
}

// TestChatHistoryHTTPRoundTrip drives the routes the page actually calls.
func TestChatHistoryHTTPRoundTrip(t *testing.T) {
	h := New(Options{DataDir: t.TempDir()})

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/chat/conversations",
		`{"title":"网关测试","client":"trae","model":"trae/deepseek"}`)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", rec.Code, rec.Body.String())
	}
	var created ChatThread
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.ID == "" || created.Title != "网关测试" {
		t.Fatalf("created = %+v", created)
	}

	rec = hitRoute(t, h, http.MethodPatch, "/panel/api/chat/conversations/"+created.ID,
		`{"messages":[{"role":"user","content":"第一问"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body %s", rec.Code, rec.Body.String())
	}

	rec = hitRoute(t, h, http.MethodGet, "/panel/api/chat/conversations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Conversations []ChatThread `json:"conversations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Conversations) != 1 || len(list.Conversations[0].Messages) != 1 {
		t.Fatalf("list = %+v", list)
	}
	if list.Conversations[0].Messages[0].Content != "第一问" {
		t.Fatalf("message lost: %+v", list.Conversations[0].Messages)
	}

	rec = hitRoute(t, h, http.MethodPatch, "/panel/api/chat/conversations/"+created.ID,
		`{"title":"改过的名字"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "改过的名字") {
		t.Fatalf("rename body = %s", rec.Body.String())
	}

	rec = hitRoute(t, h, http.MethodDelete, "/panel/api/chat/conversations/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}
	rec = hitRoute(t, h, http.MethodGet, "/panel/api/chat/conversations", "")
	if !strings.Contains(rec.Body.String(), `"conversations":[]`) {
		t.Fatalf("after delete = %s", rec.Body.String())
	}
}

func TestChatHistoryHTTPClearAll(t *testing.T) {
	h := New(Options{DataDir: t.TempDir()})
	hitRoute(t, h, http.MethodPost, "/panel/api/chat/conversations", `{"title":"A"}`)
	hitRoute(t, h, http.MethodPost, "/panel/api/chat/conversations", `{"title":"B"}`)

	rec := hitRoute(t, h, http.MethodDelete, "/panel/api/chat/conversations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body %s", rec.Code, rec.Body.String())
	}
	rec = hitRoute(t, h, http.MethodGet, "/panel/api/chat/conversations", "")
	if !strings.Contains(rec.Body.String(), `"conversations":[]`) {
		t.Fatalf("after clear = %s", rec.Body.String())
	}
}

func TestChatHistoryHTTPGuards(t *testing.T) {
	h := New(Options{DataDir: t.TempDir()})
	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"put on the collection", http.MethodPut, "/panel/api/chat/conversations", `{}`, http.StatusMethodNotAllowed},
		{"patch without an id", http.MethodPatch, "/panel/api/chat/conversations/", `{}`, http.StatusBadRequest},
		{"patch an unknown id", http.MethodPatch, "/panel/api/chat/conversations/nope", `{"title":"x"}`, http.StatusNotFound},
		{"delete an unknown id", http.MethodDelete, "/panel/api/chat/conversations/nope", "", http.StatusNotFound},
		{"create with a bad body", http.MethodPost, "/panel/api/chat/conversations", `{`, http.StatusBadRequest},
	} {
		rec := hitRoute(t, h, tc.method, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s = %d, want %d (body %s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestChatHistoryWithoutDataDirStaysInMemory(t *testing.T) {
	h := New(Options{})
	rec := hitRoute(t, h, http.MethodPost, "/panel/api/chat/conversations", `{"title":"memory"}`)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create without a data dir = %d, body %s", rec.Code, rec.Body.String())
	}
}
