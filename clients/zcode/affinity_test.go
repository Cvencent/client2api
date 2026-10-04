package zcode

import (
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// The zcode half of conversation→account stickiness.  Everything here is
// offline: the pool is built from config accounts and no request is ever sent,
// because the property under test is selection, not transport.

const affinityConfig = `{
  "auto_discover": false,
  "accounts": [
    {"id":"a1","label":"one","api_key":"sk-affinity-1","provider":"bigmodel"},
    {"id":"a2","label":"two","api_key":"sk-affinity-2","provider":"bigmodel"}
  ]
}`

func affinityClient(t *testing.T) *Client {
	t.Helper()
	isolateHome(t)
	return newTestClient(t, affinityConfig, &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		t.Fatal("the affinity tests must not reach the network")
		return nil, nil
	}})
}

// TestZcodeCapabilitiesReportConversationBinder is the assertion that keeps the
// panel's account dropdown switched on: the frontend hides the per-account
// picker unless this flag is true, so a module that implements the interface but
// fails to satisfy core's assertion would silently regress to "not supported".
func TestZcodeCapabilitiesReportConversationBinder(t *testing.T) {
	c := affinityClient(t)

	if _, ok := core.AsConversationBinder(c); !ok {
		t.Fatal("zcode does not satisfy core.ConversationBinder")
	}
	if caps := core.CapabilitiesOf(t.Context(), c); !caps.Conversations {
		t.Fatal("capabilities do not report Conversations")
	}
}

// TestZcodeBoundConversationKeepsItsAccount is the point of the feature: the
// cursor would hand out a1 first, but a bound conversation gets a2.
func TestZcodeBoundConversationKeepsItsAccount(t *testing.T) {
	c := affinityClient(t)
	model := "GLM-5.3"

	// Prove the cursor's own preference first, so the test cannot pass by
	// accident on a pool that would have chosen a2 anyway.
	if got := c.pickAccount(map[string]bool{}, "", model); got == nil || got.ID != "a1" {
		t.Fatalf("unbound pick = %v, want a1", got)
	}

	c.BindConversation("conv-1", "a2")
	got := c.pickAccount(map[string]bool{}, "conv-1", model)
	if got == nil || got.ID != "a2" {
		t.Fatalf("bound pick = %v, want a2", got)
	}
	if id, ok := c.ConversationAccount("conv-1", model); !ok || id != "a2" {
		t.Fatalf("ConversationAccount = %q,%v, want a2,true", id, ok)
	}
}

// TestZcodePickBindsTheChosenAccount covers the other half of the order: a
// conversation that arrives with no binding leaves with one, written at
// selection time so two concurrent first turns agree instead of racing.
func TestZcodePickBindsTheChosenAccount(t *testing.T) {
	c := affinityClient(t)
	model := "GLM-5.3"

	got := c.pickAccount(map[string]bool{}, "conv-new", model)
	if got == nil {
		t.Fatal("pickAccount returned nothing")
	}
	id, ok := c.ConversationAccount("conv-new", model)
	if !ok || id != got.ID {
		t.Fatalf("after picking %s the binding is %q,%v", got.ID, id, ok)
	}
}

// TestZcodeUnusableBindingIsDroppedNotServed is the safety property: stickiness
// may never be the reason a parked credential is used.  The table must drop the
// binding, and the picker must fall through to a live account.
func TestZcodeUnusableBindingIsDroppedNotServed(t *testing.T) {
	c := affinityClient(t)
	model := "GLM-5.3"

	c.BindConversation("conv-dead", "a2")
	c.pool.mark("a2", func(a *Account) {
		a.State = stateExhausted
		a.LastError = "quota exhausted"
	})

	if id, ok := c.ConversationAccount("conv-dead", model); ok {
		t.Fatalf("an exhausted account resolved as %q, want absent", id)
	}
	if n := c.affinity.Count(); n != 0 {
		t.Fatalf("the dead binding survived: %d entries", n)
	}
	got := c.pickAccount(map[string]bool{}, "conv-dead", model)
	if got == nil || got.ID != "a1" {
		t.Fatalf("pick = %v, want the live a1", got)
	}
}

// TestZcodeJWTBindingWithoutACaptchaSolverIsNotUsable pins the module-specific
// edge: pool.selectableLocked refuses a JWT account while no captcha solver is
// configured, so the affinity predicate must refuse it too.  Anything looser
// would let stickiness select a credential whose request is guaranteed to fail
// with core.ErrNotConfigured.
func TestZcodeJWTBindingWithoutACaptchaSolverIsNotUsable(t *testing.T) {
	isolateHome(t)
	cfg := `{
      "auto_discover": false,
      "accounts": [
        {"id":"k1","api_key":"sk-affinity-1","provider":"bigmodel"},
        {"id":"j1","mode":"jwt","jwt":"` + makeJWT(`{"sub":"u1"}`) + `","provider":"bigmodel"}
      ]
    }`
	c := newTestClient(t, cfg, &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		t.Fatal("must not reach the network")
		return nil, nil
	}})
	model := "GLM-5.3"

	if c.pool.usableByID("j1") {
		t.Fatal("a jwt account is usable with no captcha_command configured")
	}
	c.BindConversation("conv-jwt", "j1")
	if id, ok := c.ConversationAccount("conv-jwt", model); ok {
		t.Fatalf("a jwt account with no solver resolved as %q", id)
	}
	if got := c.pickAccount(map[string]bool{}, "conv-jwt", model); got == nil || got.ID != "k1" {
		t.Fatalf("pick = %v, want the api-key account", got)
	}
}

// TestZcodeUnbindForgetsTheBinding covers the administrative hook the panel
// route uses.
func TestZcodeUnbindForgetsTheBinding(t *testing.T) {
	c := affinityClient(t)

	c.BindConversation("conv-2", "a2")
	if !c.UnbindConversation("conv-2") {
		t.Fatal("UnbindConversation reported nothing to forget")
	}
	if c.UnbindConversation("conv-2") {
		t.Fatal("UnbindConversation forgot the same key twice")
	}
	if _, ok := c.ConversationAccount("conv-2", "GLM-5.3"); ok {
		t.Fatal("the binding survived UnbindConversation")
	}
	if c.UnbindConversation("") {
		t.Fatal("an empty key reported a binding")
	}
}

// TestZcodeEmptyKeyTouchesNothing keeps the "not conversation-scoped" path
// honest: a request with no key must behave exactly as the cursor did before
// this feature existed, and must not populate the table.
func TestZcodeEmptyKeyTouchesNothing(t *testing.T) {
	c := affinityClient(t)

	got := c.pickAccount(map[string]bool{}, "", "GLM-5.3")
	if got == nil || got.ID != "a1" {
		t.Fatalf("pick = %v, want a1", got)
	}
	if n := c.affinity.Count(); n != 0 {
		t.Fatalf("an unscoped request wrote %d bindings", n)
	}
}

// TestZcodeSkippedBindingFallsThroughAndRebinds: once the rotation loop has
// excluded the bound account, the binding must not be re-served — that would
// burn an attempt on a credential the loop already proved bad for this request.
func TestZcodeSkippedBindingFallsThroughAndRebinds(t *testing.T) {
	c := affinityClient(t)
	model := "GLM-5.3"

	c.BindConversation("conv-3", "a1")
	skip := map[string]bool{"a1": true}

	got := c.pickAccount(skip, "conv-3", model)
	if got == nil || got.ID != "a2" {
		t.Fatalf("pick = %v, want a2", got)
	}
	if !skip["a2"] {
		t.Fatal("the chosen account was not added to skip")
	}
	if id, ok := c.ConversationAccount("conv-3", model); !ok || id != "a2" {
		t.Fatalf("the conversation did not move on: %q,%v", id, ok)
	}
}

// TestZcodeResolvedAccountIsAlsoSkipped checks the bound path records the
// credential it used, so a retry cannot loop on it.
func TestZcodeResolvedAccountIsAlsoSkipped(t *testing.T) {
	c := affinityClient(t)

	c.BindConversation("conv-4", "a2")
	skip := map[string]bool{}
	got := c.pickAccount(skip, "conv-4", "GLM-5.3")
	if got == nil || got.ID != "a2" {
		t.Fatalf("pick = %v, want a2", got)
	}
	if !skip["a2"] {
		t.Fatal("the bound account was not recorded in skip")
	}
}

// TestZcodeApplyLiveRetunesTheWindow covers the reload path, including the rule
// that a zero value means "the file said nothing" and must not pin the table to
// a zero window.
func TestZcodeApplyLiveRetunesTheWindow(t *testing.T) {
	c := affinityClient(t)

	if got := c.affinity.TTL(); got != core.DefaultAffinityTTL {
		t.Fatalf("initial ttl = %v, want the shared default", got)
	}

	c.ApplyLive(core.LiveSettings{AffinityTTL: time.Minute, AffinityGCInterval: 2 * time.Minute})
	if got := c.affinity.TTL(); got != time.Minute {
		t.Fatalf("ttl after ApplyLive = %v, want 1m", got)
	}
	if got := c.affinity.GCInterval(); got != 2*time.Minute {
		t.Fatalf("gc interval after ApplyLive = %v, want 2m", got)
	}

	// A partial settings value keeps what is in force.
	c.ApplyLive(core.LiveSettings{})
	if got := c.affinity.TTL(); got != time.Minute {
		t.Fatalf("an empty ApplyLive changed the ttl to %v", got)
	}
	if got := c.affinity.GCInterval(); got != 2*time.Minute {
		t.Fatalf("an empty ApplyLive changed the gc interval to %v", got)
	}
}

// TestZcodeUsableForMatchesThePicker is the invariant the whole feature rests
// on: the affinity predicate and the cursor picker must agree on who may serve.
// A divergence here is invisible in normal traffic and shows up only as
// stickiness serving a parked account.
func TestZcodeUsableForMatchesThePicker(t *testing.T) {
	c := affinityClient(t)

	c.pool.mark("a1", func(a *Account) { a.State = stateInvalid })
	c.pool.mark("a2", func(a *Account) {
		a.State = stateCooling
		a.CooldownUntil = time.Now().Add(time.Hour)
	})

	usable := c.usableFor("GLM-5.3")
	for _, a := range c.pool.selectableAccounts() {
		if !usable(a.ID) {
			t.Fatalf("the picker offers %s but the affinity predicate refuses it", a.ID)
		}
	}
	if usable("a1") || usable("a2") || usable("") || usable("nope") {
		t.Fatalf("the predicate admitted an account the picker refuses: %v %v %v %v",
			usable("a1"), usable("a2"), usable(""), usable("nope"))
	}
}
