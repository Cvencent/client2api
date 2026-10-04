package minimaxcode

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// conversation stickiness
//
// The fixture below is the one every test here needs: two credentials whose
// tokens are distinguishable on the wire, so "which account served this turn" is
// answered by the captured request rather than by the pool's own bookkeeping.
// Reusing minimaxcode_test.go's helpers (newTestClient, fakeTransport, reply,
// drain) keeps this file honest about the module's real behaviour.
// ---------------------------------------------------------------------------

func twoAccounts() map[string]any {
	return map[string]any{
		"accounts": []map[string]any{
			{"id": "acct-1", "label": "First", "access_token": "mmoat_one"},
			{"id": "acct-2", "label": "Second", "access_token": "mmoat_two"},
		},
	}
}

// servedBy is the token the i-th upstream request carried.
func servedBy(t *testing.T, ft *fakeTransport, i int) string {
	t.Helper()
	req := ft.requestAt(i)
	if req == nil {
		t.Fatalf("upstream saw %d requests, so there is no request #%d", ft.count(), i)
	}
	return strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
}

// sayHi runs one turn of a conversation through Chat and drains the answer.
// opts is the "client2api" options object, which is where a conversation key
// lives; nil means "not conversation-scoped".
func sayHi(t *testing.T, c *Client, opts map[string]any) {
	t.Helper()
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "MiniMax-M3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Options:  opts,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	drain(t, stream)
}

// TestABoundConversationKeepsItsAccount is the point of the whole feature: a
// conversation pinned to acct-2 is served by acct-2, even though the rotation's
// cursor is still sitting on acct-1 and would otherwise pick it.
func TestABoundConversationKeepsItsAccount(t *testing.T) {
	c, ft := newTestClient(t, twoAccounts(), func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, reply), nil
	})

	// Bind to the SECOND account.  The cursor starts at zero, so an unbound
	// pick here is acct-1 -- which is what makes this test able to fail.
	c.BindConversation("conv-1", "acct-2")
	if got, ok := c.ConversationAccount("conv-1", "MiniMax-M3"); !ok || got != "acct-2" {
		t.Fatalf("ConversationAccount = (%q, %v), want (acct-2, true)", got, ok)
	}

	sayHi(t, c, map[string]any{"conversation_id": "conv-1"})
	if got := servedBy(t, ft, 0); got != "mmoat_two" {
		t.Fatalf("the bound conversation was served by %q, want mmoat_two", got)
	}

	// A later turn of the same conversation lands on the same credential.
	sayHi(t, c, map[string]any{"conversation_id": "conv-1"})
	if got := servedBy(t, ft, 1); got != "mmoat_two" {
		t.Fatalf("turn two was served by %q, want mmoat_two", got)
	}

	// And stickiness did not move the rotation: an unbound request still gets
	// the account the cursor was pointing at all along, so a bound conversation
	// cannot starve the pool or skew the rotation for everyone else.
	sayHi(t, c, nil)
	if got := servedBy(t, ft, 2); got != "mmoat_one" {
		t.Fatalf("an unbound request was served by %q, want mmoat_one", got)
	}
}

// TestABindingToACoolingAccountFallsThrough proves a binding is an optimisation,
// never a constraint: once the pinned credential is cooling, the binding is
// dropped, the request is served by the other account, and the conversation is
// re-pinned to it.
func TestABindingToACoolingAccountFallsThrough(t *testing.T) {
	c, ft := newTestClient(t, twoAccounts(), func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, reply), nil
	})

	c.BindConversation("conv-2", "acct-1")

	// Cool the bound account exactly the way a rate limit does.
	c.pool.mark("acct-1", func(a *Account) {
		a.State = stateCooling
		a.CooldownUntil = time.Now().Add(time.Hour)
	})

	// The binding is now a lie, and asking about it retires it.
	if got, ok := c.ConversationAccount("conv-2", "MiniMax-M3"); ok {
		t.Fatalf("ConversationAccount = (%q, true), want the dead binding reported absent", got)
	}

	// The request still succeeds -- on the other credential, not on the cooling
	// one, and not as an error.
	sayHi(t, c, map[string]any{"conversation_id": "conv-2"})
	if got := servedBy(t, ft, 0); got != "mmoat_two" {
		t.Fatalf("the fallthrough served %q, want mmoat_two", got)
	}
	if got, ok := c.ConversationAccount("conv-2", "MiniMax-M3"); !ok || got != "acct-2" {
		t.Fatalf("after the fallthrough ConversationAccount = (%q, %v), want (acct-2, true)", got, ok)
	}
}

// TestABindingToADisabledAccountFallsThrough is the same rule for the operator's
// own switch: disabling a credential in the panel must also retire the
// conversations pointing at it.
func TestABindingToADisabledAccountFallsThrough(t *testing.T) {
	c, ft := newTestClient(t, twoAccounts(), func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, reply), nil
	})

	c.BindConversation("conv-3", "acct-1")
	if err := c.SetAccountEnabled(context.Background(), "acct-1", false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}

	if got, ok := c.ConversationAccount("conv-3", "MiniMax-M3"); ok {
		t.Fatalf("ConversationAccount = (%q, true) for a disabled account", got)
	}

	sayHi(t, c, map[string]any{"conversation_id": "conv-3"})
	if got := servedBy(t, ft, 0); got != "mmoat_two" {
		t.Fatalf("the fallthrough served %q, want mmoat_two", got)
	}
	if got, ok := c.ConversationAccount("conv-3", "MiniMax-M3"); !ok || got != "acct-2" {
		t.Fatalf("after the fallthrough ConversationAccount = (%q, %v), want (acct-2, true)", got, ok)
	}
}

func TestConversationAccountReportsAbsentForAnUnknownKey(t *testing.T) {
	c, _ := newTestClient(t, oneAccount(), nil)

	if got, ok := c.ConversationAccount("never-seen", "MiniMax-M3"); ok || got != "" {
		t.Fatalf("ConversationAccount = (%q, %v), want (\"\", false)", got, ok)
	}
	// The module must not invent a key: no options and no user is not a
	// conversation, and a request with no key must leave the table untouched.
	if got, ok := c.ConversationAccount("", "MiniMax-M3"); ok || got != "" {
		t.Fatalf("ConversationAccount(\"\") = (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestUnbindConversationForgetsTheKey(t *testing.T) {
	c, _ := newTestClient(t, twoAccounts(), nil)

	c.BindConversation("conv-4", "acct-1")
	if !c.UnbindConversation("conv-4") {
		t.Fatal("UnbindConversation reported nothing to forget for a key that was bound")
	}
	if got, ok := c.ConversationAccount("conv-4", "MiniMax-M3"); ok {
		t.Fatalf("ConversationAccount = (%q, true) after Unbind", got)
	}
	if c.UnbindConversation("conv-4") {
		t.Fatal("UnbindConversation reported forgetting a key that was already gone")
	}
}

// TestConversationKeyReadsTheSpellingsTheGatewaySends pins the accepted
// spellings, because the wiring is only as good as the key it extracts.
func TestConversationKeyReadsTheSpellingsTheGatewaySends(t *testing.T) {
	cases := []struct {
		name string
		req  *core.ChatRequest
		want string
	}{
		{"conversation_id", &core.ChatRequest{Options: map[string]any{"conversation_id": "c1"}}, "c1"},
		{"conversationId", &core.ChatRequest{Options: map[string]any{"conversationId": "c2"}}, "c2"},
		{"prompt_cache_key", &core.ChatRequest{Options: map[string]any{"prompt_cache_key": "c3"}}, "c3"},
		{"user", &core.ChatRequest{User: "  u1  "}, "u1"},
		{"options without a key", &core.ChatRequest{Options: map[string]any{"temperature": 0.2}}, ""},
		{"nothing at all", &core.ChatRequest{}, ""},
		{"nil request", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := conversationKey(tc.req); got != tc.want {
				t.Fatalf("conversationKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestApplyLiveMovesTheStickinessWindow covers the operator-facing knob.  A zero
// in the live config means "the file said nothing", so it must leave the window
// alone rather than pin it to nothing.
func TestApplyLiveMovesTheStickinessWindow(t *testing.T) {
	c, _ := newTestClient(t, oneAccount(), nil)

	def := c.affinity.TTL()
	if def <= 0 {
		t.Fatalf("the default stickiness window is %s, which would disable the feature by default", def)
	}

	c.ApplyLive(core.LiveSettings{AffinityTTL: 11 * time.Minute, AffinityGCInterval: 90 * time.Second})
	if got := c.affinity.TTL(); got != 11*time.Minute {
		t.Fatalf("TTL = %s, want 11m", got)
	}
	if got := c.affinity.GCInterval(); got != 90*time.Second {
		t.Fatalf("GC interval = %s, want 90s", got)
	}

	c.ApplyLive(core.LiveSettings{})
	if got := c.affinity.TTL(); got != 11*time.Minute {
		t.Fatalf("TTL = %s after an empty live config, want it left at 11m", got)
	}
	if got := c.affinity.GCInterval(); got != 90*time.Second {
		t.Fatalf("GC interval = %s after an empty live config, want it left at 90s", got)
	}
}

// TestTheConversationCapabilityIsAdvertised is the contract check: the gateway
// lights the feature up by type assertion, so a module that implements the
// methods but is not seen as a ConversationBinder would silently lose the panel
// switch.
func TestTheConversationCapabilityIsAdvertised(t *testing.T) {
	c, _ := newTestClient(t, oneAccount(), nil)

	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Conversations {
		t.Fatal("CapabilitiesOf did not advertise Conversations; core is not seeing this module as a ConversationBinder")
	}
	var binder core.ConversationBinder = c
	if binder == nil {
		t.Fatalf("the client does not satisfy core.ConversationBinder (it is a %T)", c)
	}
}
