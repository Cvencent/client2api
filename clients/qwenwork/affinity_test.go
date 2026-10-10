package qwenwork

// affinity_test.go — the module half of conversation→account stickiness.
//
// These cases reuse the existing fixtures rather than inventing a parallel set:
// panelClient builds a real Client (accounts_test.go), fakeTransport keeps the
// chat path offline and records what was sent (qwenwork_test.go), and
// chatSSEFrames is the same canned upstream stream the retry tests use.  The
// point of the suite is not just that the table binds and unbinds, but that the
// key Chat derives actually reaches the picker: a stickiness feature that is
// wired into nothing passes every table-level test and does nothing.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// twoAccountConfig is the fixture every case here starts from: two usable
// credentials with the same (zero) LRU stamp, so the pool's stable sort hands
// the first request to uid:1 and, once uid:1 has been used, the next unbound
// request to uid:2.  That asymmetry is what makes a dead picker detectable.
const twoAccountConfig = `{"accounts":[` +
	`{"uid":"1","nickname":"first","access_token":"tok-1"},` +
	`{"uid":"2","nickname":"second","access_token":"tok-2"}]}`

// affinityClient is a real Client over a throwaway data dir, with the affinity
// table New is supposed to have built.
func affinityClient(t *testing.T) *Client {
	t.Helper()
	c := panelClient(t, twoAccountConfig, nil)
	if c.affinity == nil {
		t.Fatal("New did not build the affinity table")
	}
	return c
}

// TestQwenworkBoundConversationPicksItsAccount is the headline case: a
// conversation pinned to the account that would otherwise lose the LRU race is
// served by that account anyway.
func TestQwenworkBoundConversationPicksItsAccount(t *testing.T) {
	c := affinityClient(t)

	// Control: with no conversation key the pool's own pick takes the first
	// configured account, which is the one the binding below must beat.
	if e := c.pickAccount(nil, "", "pro"); e == nil || e.acct.id() != "uid:1" {
		t.Fatalf("unbound pick = %v, want uid:1", e)
	}

	c.BindConversation("conv-1", "uid:2")
	e := c.pickAccount(nil, "conv-1", "pro")
	if e == nil || e.acct.id() != "uid:2" {
		t.Fatalf("bound pick = %v, want the bound uid:2", e)
	}
	if got, ok := c.ConversationAccount("conv-1", "pro"); !ok || got != "uid:2" {
		t.Errorf("ConversationAccount = %q,%v, want \"uid:2\",true", got, ok)
	}

	// A skip entry means the account already failed in this request, so the
	// binding must not be resolved back in.
	if e := c.pickAccount(map[string]bool{"uid:2": true}, "conv-1", "pro"); e == nil || e.acct.id() != "uid:1" {
		t.Errorf("pick with the bound account skipped = %v, want the fall-through uid:1", e)
	}
}

// TestQwenworkUnusableBindingIsDroppedAndThePickerFallsThrough covers the
// cooling case: a binding that has gone bad is reported absent, the picker
// falls through to a healthy account, and the conversation is re-bound to it
// rather than failing.
func TestQwenworkUnusableBindingIsDroppedAndThePickerFallsThrough(t *testing.T) {
	c := affinityClient(t)
	c.BindConversation("conv-cool", "uid:1")

	// Park the bound account exactly the way a quota failure would.
	e := c.pool.find("uid:1")
	if e == nil {
		t.Fatal("uid:1 is not in the pool")
	}
	e.park(time.Now().Add(time.Hour), stateExhausted)

	if got, ok := c.ConversationAccount("conv-cool", "pro"); ok {
		t.Errorf("a parked binding resolved as %q; it must be reported absent", got)
	}
	picked := c.pickAccount(nil, "conv-cool", "pro")
	if picked == nil || picked.acct.id() != "uid:2" {
		t.Fatalf("pick after a parked binding = %v, want the fall-through uid:2", picked)
	}
	// The fall-through re-binds, so the conversation is sticky again -- to the
	// account that can actually serve it.
	if got, ok := c.ConversationAccount("conv-cool", "pro"); !ok || got != "uid:2" {
		t.Errorf("re-bound account = %q,%v, want \"uid:2\",true", got, ok)
	}
}

// TestQwenworkDisabledBindingIsDropped is the same rule for the other way an
// account becomes unusable: an operator disabling it.
func TestQwenworkDisabledBindingIsDropped(t *testing.T) {
	c := affinityClient(t)
	c.BindConversation("conv-off", "uid:1")

	e := c.pool.find("uid:1")
	if e == nil {
		t.Fatal("uid:1 is not in the pool")
	}
	e.acct.Disabled = true

	if _, ok := c.ConversationAccount("conv-off", "pro"); ok {
		t.Error("a disabled account must not resolve as a binding")
	}
	if picked := c.pickAccount(nil, "conv-off", "pro"); picked == nil || picked.acct.id() != "uid:2" {
		t.Errorf("pick = %v, want uid:2", picked)
	}
}

// TestQwenworkUnknownConversationKeyIsAbsent pins the "no answer means pick
// normally, never fail" half of the contract, including the empty key.
func TestQwenworkUnknownConversationKeyIsAbsent(t *testing.T) {
	c := affinityClient(t)

	if got, ok := c.ConversationAccount("never-seen", "pro"); ok || got != "" {
		t.Errorf("ConversationAccount(unknown) = %q,%v, want \"\",false", got, ok)
	}

	// An empty key is not a conversation and must never be stored.
	c.BindConversation("", "uid:1")
	if c.affinity.Count() != 0 {
		t.Errorf("binding the empty key stored %d entries, want 0", c.affinity.Count())
	}
	if _, ok := c.ConversationAccount("", "pro"); ok {
		t.Error("the empty key must never resolve")
	}
}

// TestQwenworkUnbindForgetsTheBinding covers the administrative hook and its
// return value, then proves the picker really did forget.
func TestQwenworkUnbindForgetsTheBinding(t *testing.T) {
	c := affinityClient(t)
	c.BindConversation("conv-x", "uid:1")

	if !c.UnbindConversation("conv-x") {
		t.Fatal("UnbindConversation reported nothing to forget for a bound key")
	}
	if c.UnbindConversation("conv-x") {
		t.Error("a second UnbindConversation must report false")
	}
	if _, ok := c.ConversationAccount("conv-x", "pro"); ok {
		t.Error("the binding survived UnbindConversation")
	}
	if picked := c.pickAccount(nil, "conv-x", "pro"); picked == nil || picked.acct.id() != "uid:1" {
		t.Errorf("pick = %v, want the LRU uid:1", picked)
	}
}

// TestQwenworkChatKeepsAConversationOnOneAccount is the wiring test.  It goes
// through Chat, not through pickAccount, because dead wiring is the failure mode
// being checked: after the first turn the LRU picker would take uid:2, so a
// second turn that stays on uid:1 can only be the binding being consulted.
func TestQwenworkChatKeepsAConversationOnOneAccount(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		return fakeResponse(req, http.StatusOK, chatSSEFrames), nil
	}
	c := panelClient(t, twoAccountConfig, rt)

	for turn := 0; turn < 2; turn++ {
		stream, err := c.Chat(context.Background(), &core.ChatRequest{
			Model:    "pro",
			Messages: []core.Message{{Role: "user", Content: "hello"}},
			Options:  map[string]any{"conversation_id": "conv-e2e"},
		})
		if err != nil {
			t.Fatalf("turn %d: Chat: %v", turn, err)
		}
		stream.Close()
	}

	first, second := c.pool.find("uid:1"), c.pool.find("uid:2")
	if first == nil || second == nil {
		t.Fatal("the pool lost an account")
	}
	if first.acct.LastUsed == 0 {
		t.Fatal("the first turn never served uid:1")
	}
	if second.acct.LastUsed != 0 {
		t.Fatal("the second turn moved off the conversation's account: the key is not wired into the picker")
	}
	// Control: at this point the LRU picker would take uid:2, so staying on
	// uid:1 can only have been the binding.  Without this the test would pass
	// even if the pool happened to prefer uid:1 anyway.
	if e := c.pool.pick(nil); e == nil || e.acct.id() != "uid:2" {
		t.Fatalf("control: LRU pick = %v, want uid:2 -- this test cannot detect a dead picker", e)
	}
}

// TestQwenworkChatWithoutAConversationKeyUsesContentFallback pins the new
// default for every platform: a request that names no conversation derives a
// stable key from the first user turn and stays on the account it warmed.
func TestQwenworkChatWithoutAConversationKeyUsesContentFallback(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		return fakeResponse(req, http.StatusOK, chatSSEFrames), nil
	}
	c := panelClient(t, twoAccountConfig, rt)

	for turn := 0; turn < 2; turn++ {
		stream, err := c.Chat(context.Background(), &core.ChatRequest{
			Model:    "pro",
			Messages: []core.Message{{Role: "user", Content: "hello"}},
		})
		if err != nil {
			t.Fatalf("turn %d: Chat: %v", turn, err)
		}
		stream.Close()
	}

	if second := c.pool.find("uid:2"); second == nil || second.acct.LastUsed != 0 {
		t.Error("a repeated unscoped conversation must stay on its first account")
	}
	if got := c.affinity.Count(); got != 1 {
		t.Errorf("an unscoped conversation left %d bindings, want 1", got)
	}
}

// TestQwenworkApplyLiveTunesTheAffinityWindow pins the LiveReloader half: the
// two stickiness settings are applied, and a partial reload (both zero) leaves
// the table's own defaults alone instead of pinning it to zero.
func TestQwenworkApplyLiveTunesTheAffinityWindow(t *testing.T) {
	c := affinityClient(t)

	c.ApplyLive(core.LiveSettings{})
	if got := c.affinity.TTL(); got != core.DefaultAffinityTTL {
		t.Errorf("a partial reload set the TTL to %s, want the default %s", got, core.DefaultAffinityTTL)
	}

	c.ApplyLive(core.LiveSettings{AffinityTTL: 3 * time.Minute, AffinityGCInterval: 30 * time.Second})
	if got := c.affinity.TTL(); got != 3*time.Minute {
		t.Errorf("TTL = %s, want 3m", got)
	}
	if got := c.affinity.GCInterval(); got != 30*time.Second {
		t.Errorf("GC interval = %s, want 30s", got)
	}
}

// TestQwenworkAdvertisesTheConversationBinder pins the type-assertion
// capability the panel reads.
func TestQwenworkAdvertisesTheConversationBinder(t *testing.T) {
	c := affinityClient(t)
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Conversations {
		t.Error("qwenwork implements core.ConversationBinder but Capabilities says otherwise")
	}
	if !caps.Live {
		t.Error("qwenwork implements core.LiveReloader but Capabilities says otherwise")
	}
}
