package core

import (
	"strings"
	"testing"
)

// ConversationKeyOf is the shared stickiness key resolver.  It must prefer an
// explicit conversation id, but when the caller names nothing it has to derive
// the same stable key WorkBuddy already uses, so every platform can keep a
// conversation on the account that warmed its prompt cache.
func TestConversationKeyOfFallsBackToContent(t *testing.T) {
	req := &ChatRequest{Messages: []Message{{Role: "user", Content: "hello"}}}
	got := ConversationKeyOf(req)
	if got == "" {
		t.Fatal("ConversationKeyOf returned empty for a signable first user turn")
	}
	if got != DeriveConversationKey(req.Messages) {
		t.Fatalf("ConversationKeyOf = %q, want the same key DeriveConversationKey returns", got)
	}
	if !strings.HasPrefix(got, DerivedKeyPrefix) {
		t.Fatalf("ConversationKeyOf = %q, want a %q-derived key", got, DerivedKeyPrefix)
	}
}

// The fallback must degrade to "no key" instead of inventing a conversation
// when there is nothing stable to hash.
func TestConversationKeyOfStaysOffWhenNothingIsSignable(t *testing.T) {
	for _, req := range []*ChatRequest{
		nil,
		{},
		{Messages: []Message{{Role: "assistant", Content: "unattributed"}}},
	} {
		if got := ConversationKeyOf(req); got != "" {
			t.Fatalf("ConversationKeyOf(%+v) = %q, want empty", req, got)
		}
	}
}
