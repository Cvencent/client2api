package gateway

import (
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// 「最近调用」那一列会话ID 的来源。
//
// 现象：同一个客户端（workbuddy）的调用记录里，一部分行有会话ID、一部分没有。
// 原因不是记录逻辑时有时无，而是"调用方到底有没有报会话 id"本来就不统一：
// 走 OpenAI 兼容入口的客户端会带 conversation_id / metadata.conversation_id，
// 而直接从面板"对话测试"或某些 IDE 插件发起的请求一个都不带。
//
// sessionIDFor 只按"调用方真的报了会话"来找，找不到就是空字符串——这个判断本身
// 是对的，不能改成拿 user 或内容哈希去凑数（那会让这一列谎称一个不存在的会话）。
// 真正的问题在别处：core 里已经有一个 DeriveConversationKey，能在没有任何会话 id
// 时从消息内容推一个带 "d-" 前缀的稳定键，而 gateway 这一侧从没用过它。于是同一个
// 上游会话，前半段有 id、后半段没有，同一张表里两行看起来像两个毫不相干的对话。
//
// 这里钉住的是"来源优先级 + 一个都不能少"：显式 id 永远赢；没有显式 id 时用推导
// 键补上，并且带上可识别的 "d-" 前缀，读者一眼能看出这不是调用方报的 id。
// ---------------------------------------------------------------------------

func TestSessionIDFallsBackToTheDerivedConversationKey(t *testing.T) {
	msgs := []core.Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello there"},
	}
	base := &core.ChatRequest{Messages: msgs}

	t.Run("显式会话 id 仍然优先，不被推导键覆盖", func(t *testing.T) {
		req := *base
		req.ConversationID = "conv-explicit"
		if got := sessionIDFor(&req); got != "conv-explicit" {
			t.Fatalf("sessionIDFor = %q, want the caller's own conv-explicit", got)
		}
	})

	t.Run("没有会话 id 时用推导键补上", func(t *testing.T) {
		if got := sessionIDFor(base); got == "" {
			t.Fatal("sessionIDFor returned empty for a request that names no session; 最近调用里这一行就永远是空的")
		}
		if got := derivedSessionID(base); got == "" {
			t.Fatal("derivedSessionID returned empty for a signable first user turn")
		}
	})

	t.Run("推导键带 d- 前缀，读者能看出它不是调用方报的", func(t *testing.T) {
		got := derivedSessionID(base)
		if len(got) < 2 || got[:2] != core.DerivedKeyPrefix {
			t.Fatalf("derived key = %q, want the %q prefix", got, core.DerivedKeyPrefix)
		}
	})

	t.Run("同一会话的两轮推导同一个键", func(t *testing.T) {
		second := &core.ChatRequest{Messages: []core.Message{
			{Role: "system", Content: "you are helpful"},
			{Role: "user", Content: "hello there"},
			{Role: "assistant", Content: "hi"},
			{Role: "user", Content: "and again"},
		}}
		if derivedSessionID(base) != derivedSessionID(second) {
			t.Fatal("the same conversation derived two different keys; 历史一长就会变成另一行")
		}
	})

	t.Run("不同会话不会撞键", func(t *testing.T) {
		other := &core.ChatRequest{Messages: []core.Message{
			{Role: "system", Content: "you are helpful"},
			{Role: "user", Content: "a completely different question"},
		}}
		if derivedSessionID(base) == derivedSessionID(other) {
			t.Fatal("two different conversations derived the same key")
		}
	})

	t.Run("没有可签名的用户消息时仍然留空，不硬凑", func(t *testing.T) {
		empty := &core.ChatRequest{Messages: []core.Message{{Role: "assistant", Content: "unattributed"}}}
		if got := derivedSessionID(empty); got != "" {
			t.Fatalf("derivedSessionID = %q, want empty: 没有任何用户内容可签名的请求不该被编一个会话", got)
		}
	})
}
