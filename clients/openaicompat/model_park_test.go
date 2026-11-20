package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// issue #264：rzol 的 Deepseek-v4.1-flash-1017 挂在无权限分组上，403「No
// permission to access group …」按 FailureModelRefused 分类（不改账号状态、
// 正确切平台），但账号恒 ready、无任何记忆——deepseek 轮转链上每个请求都先
// 白撞一次 403 再改道。修法 = 模块级 (账号,模型) 短停：refused 时停该对，
// 窗口内后续请求直接得到 FailureModelRefused（网关零上游调用切平台），成功
// 或模型扫描即清。四条分支全部钉住。
// ---------------------------------------------------------------------------

// groupRefusalServer answers the group-permission 403 for the doomed model and
// serves SSE for everything else.
// acceptDoomed, when true, stops the doomed model's refusal: the "vendor
// fixed the group" half of the clear-on-success test.  A plain flag is
// deliberate -- the failure being modelled is deterministic, not timed.
var acceptDoomed atomic.Bool

func groupRefusalServer(doomedModel string, hits *int64) *httptest.Server {
	acceptDoomed.Store(false)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hits, 1)
		var body struct {
			Model string `json:"model"`
		}
		raw, rerr := io.ReadAll(io.LimitReader(r.Body, 8192))
		_ = r.Body.Close()
		if rerr == nil {
			_ = json.Unmarshal(raw, &body)
		}
		if acceptDoomed.Load() {
			body.Model = ""
		}
		if body.Model == doomedModel {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"No permission to access group Lite 日卡 (request id: xx)"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
}

// TestAPairParkFollowsAModelRefusal 钉住主链路：第一次 403 记 (账号,模型) 停；
// 第二次请求窗口内直接得到 FailureModelRefused 且上游零调用；账号状态不受
// 影响（403 分组权限按设计不打死账号）。
func TestAPairParkFollowsAModelRefusal(t *testing.T) {
	const doomed = "Deepseek-v4.1-flash-1017"
	var hits int64
	srv := groupRefusalServer(doomed, &hits)
	defer srv.Close()

	s, err := newSource("rzol", core.SourceConfig{BaseURL: srv.URL}, core.Deps{
		DataDir:    t.TempDir(),
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"api_key": "key-a", "models": doomed + ",other-model"},
	}); err != nil {
		t.Fatal(err)
	}

	// 第一次：真出网吃 403，分类为 model_refused。
	req := &core.ChatRequest{Model: doomed, Messages: []core.Message{{Role: "user", Content: "hi"}}}
	_, err = s.Chat(context.Background(), req)
	f, ok := core.AsFailure(err)
	if !ok || f.Kind != core.FailureModelRefused {
		t.Fatalf("first chat err = %v, want a model_refused failure", err)
	}
	if atomic.LoadInt64(&hits) != 1 {
		t.Fatalf("hits = %d, want 1 after the first refusal", atomic.LoadInt64(&hits))
	}
	// 账号不被打死：分组权限拒绝不涉及凭据。
	if recs, _ := s.Accounts(context.Background()); len(recs) != 1 || recs[0].State != "ready" {
		t.Fatalf("account state = %+v, want ready: a group-permission 403 is not a credential verdict", recs)
	}

	// 第二次：窗口内直接得到 model_refused，上游零调用。
	_, err = s.Chat(context.Background(), req)
	f2, ok := core.AsFailure(err)
	if !ok || f2.Kind != core.FailureModelRefused {
		t.Fatalf("second chat err = %v, want a model_refused failure from the park", err)
	}
	if atomic.LoadInt64(&hits) != 1 {
		t.Fatalf("hits = %d, want 1: the parked pair must not re-ask the vendor (issue #264)", atomic.LoadInt64(&hits))
	}
	if !strings.Contains(err.Error(), "parked") {
		t.Fatalf("refusal text %q should say the pair is parked", err.Error())
	}

	// 同一账号的其它模型不受影响：served 请求照常出网。
	var served int64 = atomic.LoadInt64(&hits)
	stream, err := s.Chat(context.Background(), &core.ChatRequest{
		Model: "other-model", Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("other model err = %v, want a served stream", err)
	}
	if _, _, _, _, _, err := drainStream(stream); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if atomic.LoadInt64(&hits) != served+1 {
		t.Fatalf("hits = %d, want %d: a pair park must not touch sibling models", atomic.LoadInt64(&hits), served+1)
	}
}

// TestAServedRequestClearsThePairPark 钉住反向钉：park 之后 vendor 改口径能
// 服务的第一个请求必须立刻清 park（把时钟拨过窗口，模拟窗口期满后厂商已修
// 好分组的世界），成功即清，不重付、不留残档。
func TestAServedRequestClearsThePairPark(t *testing.T) {
	const doomed = "group-model"
	var hits int64
	srv := groupRefusalServer(doomed, &hits)
	defer srv.Close()

	s, err := newSource("rzol", core.SourceConfig{BaseURL: srv.URL}, core.Deps{
		DataDir:    t.TempDir(),
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"api_key": "key-a", "models": doomed},
	}); err != nil {
		t.Fatal(err)
	}

	req := &core.ChatRequest{Model: doomed, Messages: []core.Message{{Role: "user", Content: "hi"}}}
	if _, err := s.Chat(context.Background(), req); err == nil {
		t.Fatal("first chat should hit the 403")
	}
	s.mu.Lock()
	parkLen := len(s.modelCool)
	s.mu.Unlock()
	if parkLen != 1 {
		t.Fatalf("park entries = %d, want 1 after the refusal", parkLen)
	}

	// 厂商修好分组（从现在起 doomed 也放行）且窗口已期满：这一笔必须真出网并
	// 成功，同时清掉 park。
	acceptDoomed.Store(true)
	base := s.now
	s.now = func() time.Time { return base().Add(modelParkMax + time.Minute) }
	defer func() { s.now = base }()
	stream, err := s.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("post-fix chat = %v, want a served stream", err)
	}
	if _, _, _, _, _, err := drainStream(stream); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if atomic.LoadInt64(&hits) != 2 {
		t.Fatalf("hits = %d, want 2: the served request must reach the vendor, not the park", atomic.LoadInt64(&hits))
	}
	s.mu.Lock()
	parkLen = len(s.modelCool)
	s.mu.Unlock()
	if parkLen != 0 {
		t.Fatalf("park entries = %d, want 0: a served request falsifies the pair park", parkLen)
	}
}

// TestThePairParkPublishesAndLapses 钉住两件事：
// 1. Status 的 model_cooldowns 发布该对的 timed park（面板可见）；
// 2. 后备档：10min 基础、连续拒绝翻倍、30min 封顶。
func TestThePairParkPublishesAndLapses(t *testing.T) {
	const doomed = "group-model"
	var hits int64
	srv := groupRefusalServer(doomed, &hits)
	defer srv.Close()

	s, err := newSource("rzol", core.SourceConfig{BaseURL: srv.URL}, core.Deps{
		DataDir:    t.TempDir(),
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"api_key": "key-a", "models": doomed},
	}); err != nil {
		t.Fatal(err)
	}

	accts, err := s.Accounts(context.Background())
	if err != nil || len(accts) != 1 {
		t.Fatalf("accounts = %v (err %v), want 1", accts, err)
	}
	acctID := accts[0].ID

	// 窗口内连续三次 noteModelRefusal：第一次 10min，第二次 20min，第三次封顶
	// 30min（ladder 的单元语义，不走 Chat——Chat 在窗口内根本不再出网）。
	steps := []time.Duration{modelParkBase, 2 * modelParkBase, modelParkMax}
	for i, want := range steps {
		s.noteModelRefusal(acctID, doomed)
		s.mu.Lock()
		rec := s.modelCool[modelParkKey{id: acctID, model: doomed}]
		s.mu.Unlock()
		ttl := rec.until.Sub(s.now())
		if ttl != want {
			t.Fatalf("refusal %d: park ttl = %v, want %v (ladder base 10min, cap 30min)", i+1, ttl, want)
		}
	}

	// Status 发布。
	st := s.Status(context.Background())
	if len(st.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(st.Accounts))
	}
	_ = st
	raw, ok := st.Accounts[0].Extra["model_cooldowns"]
	if !ok {
		t.Fatal("model_cooldowns missing from the account row")
	}
	parks := raw.([]map[string]any)
	found := false
	for _, p := range parks {
		if p["model"] == doomed && p["kind"] == "rate_limit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("parks = %+v, want a rate_limit entry for %s (issue #264 panel visibility)", parks, doomed)
	}
}
