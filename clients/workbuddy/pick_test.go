package workbuddy

import (
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// pickClock is an injectable clock for the pool.  Pacing and TTL behaviour are
// only observable if the tests can move time, and they must not sleep.
type pickClock struct{ at time.Time }

func (c *pickClock) now() time.Time          { return c.at }
func (c *pickClock) advance(d time.Duration) { c.at = c.at.Add(d) }

// newPickPool builds a pool whose clock the caller controls.
func newPickPool(accounts []*Auth) (*Pool, *pickClock) {
	clk := &pickClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	p := NewPool(accounts, 0)
	p.now = clk.now
	return p, clk
}

func pickAuths() (*Auth, *Auth) {
	return &Auth{AccessToken: "at-111111111111", UID: "u1"},
		&Auth{AccessToken: "at-222222222222", UID: "u2"}
}

// --- per-model cooldown ----------------------------------------------------

// TestWorkbuddyPoolParksOnlyTheParkedModel is the whole point of a model park:
// one credential losing access to one model must not take that credential out
// of rotation for every other model.
func TestWorkbuddyPoolParksOnlyTheParkedModel(t *testing.T) {
	a, b := pickAuths()
	p, clk := newPickPool([]*Auth{a, b})

	if got := p.MarkModelBlocked(a, "model-x", ModelBlockReason); got != modelBlockBaseTTL {
		t.Fatalf("first park = %v, want %v", got, modelBlockBaseTTL)
	}
	if p.UsableForModel(a.ID(), "model-x") {
		t.Fatal("the parked model must not be usable on that account")
	}
	if !p.UsableForModel(a.ID(), "model-y") {
		t.Fatal("a park must be scoped to the model it was recorded for")
	}
	if !p.Ready() {
		t.Fatal("a model park must not park the account")
	}
	for i := 0; i < 3; i++ {
		got, ok := p.PickForModel(nil, "model-x")
		if !ok || got.ID() != b.ID() {
			t.Fatalf("pick %d for the parked model = %v/%v, want %s", i, got, ok, b.ID())
		}
		clk.advance(time.Second)
	}
	// The park lapses on its own: no revocation signal exists for 11102.
	clk.advance(modelBlockBaseTTL)
	if !p.UsableForModel(a.ID(), "model-x") {
		t.Fatal("the park must lapse once its deadline passes")
	}
}

// TestWorkbuddyModelBlockBackoffGrowsToTheCap pins the geometric policy
// (base 6h, doubling, capped at 24h) borrowed from the reference.
func TestWorkbuddyModelBlockBackoffGrowsToTheCap(t *testing.T) {
	a, _ := pickAuths()
	p, _ := newPickPool([]*Auth{a})
	want := []time.Duration{
		modelBlockBaseTTL,
		2 * modelBlockBaseTTL,
		modelBlockMaxTTL,
		modelBlockMaxTTL,
		modelBlockMaxTTL,
	}
	for i, w := range want {
		if got := p.MarkModelBlocked(a, "model-x", ModelBlockReason); got != w {
			t.Fatalf("park %d = %v, want %v", i+1, got, w)
		}
	}
}

// TestWorkbuddyModelRateLimitHonoursTheVendorReset: a 6004 carries its own
// deadline, and a deadline further out than the policy allows is still capped.
func TestWorkbuddyModelRateLimitHonoursTheVendorReset(t *testing.T) {
	a, _ := pickAuths()
	p, clk := newPickPool([]*Auth{a})

	reset := clk.at.Add(10 * time.Minute)
	if got := p.MarkModelRateLimited(a, "model-x", reset, time.Minute, modelRateLimitReason); got != 10*time.Minute {
		t.Fatalf("park = %v, want the vendor's 10m window", got)
	}
	if !p.UsableForModel(a.ID(), "model-y") {
		t.Fatal("a 6004 park must be scoped to its model")
	}
	clk.advance(time.Minute)
	far := clk.at.Add(48 * time.Hour)
	if got := p.MarkModelRateLimited(a, "model-z", far, time.Minute, modelRateLimitReason); got != softModelMax {
		t.Fatalf("park = %v, want the %v cap", got, softModelMax)
	}
}

// TestWorkbuddyClearModelCooldownOnlyClearsBackoffParks: a success proves the
// model works again, which retires a 11102 backoff.  It proves nothing about a
// window the vendor dated, so that park is left to expire.
func TestWorkbuddyClearModelCooldownOnlyClearsBackoffParks(t *testing.T) {
	a, _ := pickAuths()
	p, clk := newPickPool([]*Auth{a})

	p.MarkModelBlocked(a, "blocked", ModelBlockReason)
	if !p.ClearModelCooldown(a, "blocked") {
		t.Fatal("a success must retire a model-unavailable backoff")
	}
	if !p.UsableForModel(a.ID(), "blocked") {
		t.Fatal("the retired park must stop parking the model")
	}
	p.MarkModelRateLimited(a, "dated", clk.at.Add(time.Minute), time.Minute, modelRateLimitReason)
	if p.ClearModelCooldown(a, "dated") {
		t.Fatal("a vendor-dated window must not be cleared by a success")
	}
	p.MarkSuccessForModel(a, "dated")
	if p.UsableForModel(a.ID(), "dated") {
		t.Fatal("the dated window must survive an account success")
	}
}

// TestWorkbuddyPoolSnapshotDescribesModelParks pins the structured shape the
// panel needs: a parked model arrives as {model, kind, until, reset_at}, not as
// a rendered sentence.  The panel has to label a vendor rate-limit window
// differently from a deterministic "this model is not here" refusal, and it has
// to time the first one; parsing the module's prose from JavaScript would be
// worse.
func TestWorkbuddyPoolSnapshotDescribesModelParks(t *testing.T) {
	a, _ := pickAuths()
	p, clk := newPickPool([]*Auth{a})

	p.MarkModelBlocked(a, "refused-model", ModelBlockReason)
	p.MarkModelRateLimited(a, "limited-model", clk.at.Add(2*time.Hour), time.Minute, modelRateLimitReason)

	snap := p.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	parks, ok := snap[0].Extra["model_cooldowns"].([]core.ModelPark)
	if !ok {
		t.Fatalf("model_cooldowns = %#v, want []core.ModelPark", snap[0].Extra["model_cooldowns"])
	}
	if len(parks) != 2 {
		t.Fatalf("parks = %#v, want two entries", parks)
	}
	// Sorted by model id, so the panel's render order is stable across polls.
	if parks[0].Model != "limited-model" || parks[1].Model != "refused-model" {
		t.Fatalf("parks = %#v, want a model-sorted list", parks)
	}
	limited, refused := parks[0], parks[1]
	if limited.Kind != core.ModelParkRateLimit {
		t.Errorf("limited kind = %q, want %q", limited.Kind, core.ModelParkRateLimit)
	}
	if limited.Reason != modelRateLimitReason {
		t.Errorf("limited reason = %q, want %q", limited.Reason, modelRateLimitReason)
	}
	if got, want := limited.ResetAt, clk.at.Add(2*time.Hour).UTC().Format(time.RFC3339); got != want {
		t.Errorf("limited reset_at = %q, want %q", got, want)
	}
	if limited.Until == "" {
		t.Error("a dated park must carry the instant it lapses")
	}
	if refused.Kind != core.ModelParkUnsupported {
		t.Errorf("refused kind = %q, want %q", refused.Kind, core.ModelParkUnsupported)
	}
	if refused.Until == "" {
		t.Error("a 11102 backoff must still carry the instant it lapses")
	}
}

// TestWorkbuddyModelScopedFailureDrivesTheModelPark drives the wiring end to
// end: the classified Error carries the scope, failureDetail forwards it, and
// MarkFailureForModel picks the right policy.
func TestWorkbuddyModelScopedFailureDrivesTheModelPark(t *testing.T) {
	a, b := pickAuths()
	p, clk := newPickPool([]*Auth{a, b})

	blocked := &Error{Kind: ErrModelBlocked, Status: 400, Msg: "11102", ModelScoped: true}
	kind, d := p.MarkFailureForModel(a, "model-x", blocked)
	if kind != ErrModelBlocked || d != modelBlockBaseTTL {
		t.Fatalf("11102 = (%v, %v), want (%v, %v)", kind, d, ErrModelBlocked, modelBlockBaseTTL)
	}
	if p.UsableForModel(a.ID(), "model-x") || !p.UsableForModel(a.ID(), "model-y") {
		t.Fatal("11102 must park exactly one model on exactly one account")
	}
	if !p.Ready() {
		t.Fatal("11102 must not remove the account from rotation")
	}

	scoped := &Error{
		Kind:        ErrSoftRate,
		Status:      429,
		Msg:         "6004",
		ModelScoped: true,
		ResetAt:     clk.at.Add(15 * time.Minute),
	}
	kind, d = p.MarkFailureForModel(b, "model-x", scoped)
	if kind != ErrSoftRate || d != 15*time.Minute {
		t.Fatalf("6004 = (%v, %v), want (%v, 15m)", kind, d, ErrSoftRate)
	}
	if p.UsableForModel(b.ID(), "model-x") {
		t.Fatal("the 6004 model must be parked")
	}
	if !p.UsableForModel(b.ID(), "model-y") {
		t.Fatal("the 6004 park must be scoped to its model")
	}

	// An account-level failure must still behave exactly as it did: no model
	// park is invented for it.
	if _, d := p.MarkFailureForModel(a, "model-y", &Error{Kind: ErrSoftRate, Status: 429}); d <= 0 {
		t.Fatal("an account-level soft rate must still park the account")
	}
	if e := p.findLocked(a); e == nil || len(e.modelCool) != 0 {
		t.Fatal("an account-level failure must not create a model park")
	}
}

// TestWorkbuddyPoolPickForModelRefusesWhenEveryAccountIsParked: stickiness and
// model parks are NOT only optimisations, which is the correction production
// traffic forced.  The fallback this test used to pin served a model-parked
// request from the very account the vendor had just parked for that model, so a
// busy pool re-hit the vendor's 6004 on every burst and re-recorded the park
// from "now" -- walking the reset window forward and filling the recent-calls
// view with failures against a credential that was healthy for every other
// model.  A park is the vendor's own answer, so an empty candidate set is the
// answer here.  The model-agnostic pick below is untouched, which is what keeps
// a park scoped to the one model it was recorded for.
func TestWorkbuddyPoolPickForModelRefusesWhenEveryAccountIsParked(t *testing.T) {
	a, b := pickAuths()
	p, _ := newPickPool([]*Auth{a, b})
	p.MarkModelBlocked(a, "model-x", ModelBlockReason)
	p.MarkModelBlocked(b, "model-x", ModelBlockReason)
	if got, ok := p.PickForModel(nil, "model-x"); ok {
		t.Fatalf("PickForModel = %s, want no candidate: every account is parked for that model", got.ID())
	}
	if got, ok := p.Pick(nil); !ok || got == nil {
		t.Fatal("the model-agnostic pick must keep rotating over a model park")
	}
}

// --- scored selection ------------------------------------------------------

// TestWorkbuddyPoolWeightedPickFavoursTheHealthierAccount: an account carrying
// failures takes a strictly smaller share of the rotation than a clean one.
// A plain round-robin cannot produce this split.
func TestWorkbuddyPoolWeightedPickFavoursTheHealthierAccount(t *testing.T) {
	a, b := pickAuths()
	p, clk := newPickPool([]*Auth{a, b})

	// Warm both accounts up so nobody is carrying the never-used bonus, then
	// weight a down to the floor.
	for i := 0; i < 2; i++ {
		if _, ok := p.PickForModel(nil, "model-x"); !ok {
			t.Fatal("warm-up pick failed")
		}
		clk.advance(time.Second)
	}
	ea := p.findLocked(a)
	if ea == nil {
		t.Fatal("account a vanished from the pool")
	}
	ea.fails = 4 // 1.0 - min(4*0.25, 1.0) => floored at minWeight
	// An accumulator carries its deficit across rounds, so the warm-up's own
	// deficit would swamp the 10:1 ratio under test.  Zero both and measure the
	// steady state: over enough rounds each account's share converges on its
	// weight's share of the sum.
	for _, e := range p.entries {
		e.current = 0
	}

	countA, countB := 0, 0
	for i := 0; i < 12; i++ {
		got, ok := p.PickForModel(nil, "model-x")
		if !ok {
			t.Fatal("pick failed")
		}
		if got.ID() == a.ID() {
			countA++
		} else {
			countB++
		}
		clk.advance(time.Second)
	}
	if countB <= countA {
		t.Fatalf("weights did not steer the rotation: a=%d b=%d", countA, countB)
	}
	if countA < 1 {
		t.Fatalf("the light account must never be starved entirely: a=%d b=%d", countA, countB)
	}
}

// TestWorkbuddyPoolMinPickGapSpreadsConcurrentPicks: two requests that arrive
// in the same instant must not both land on the credential the other just used.
func TestWorkbuddyPoolMinPickGapSpreadsConcurrentPicks(t *testing.T) {
	a, b := pickAuths()
	c := &Auth{AccessToken: "at-333333333333", UID: "u3"}
	p, _ := newPickPool([]*Auth{a, b, c})

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		got, ok := p.PickForModel(nil, "model-x")
		if !ok {
			t.Fatalf("pick %d failed", i)
		}
		if seen[got.ID()] {
			t.Fatalf("pick %d reused %s inside minPickGap", i, got.ID())
		}
		seen[got.ID()] = true
	}
}

// --- conversation -> account affinity --------------------------------------

// TestWorkbuddyConversationSticksToItsAccount: the second request of the same
// conversation must return to the account that already warmed the vendor's
// prompt cache, even though the scored pick would have rotated.
func TestWorkbuddyConversationSticksToItsAccount(t *testing.T) {
	a, b := pickAuths()
	p, clk := newPickPool([]*Auth{a, b})
	c := &Client{pool: p, affinity: core.NewAffinity(0)}

	req := &core.ChatRequest{User: "user-1", Options: map[string]any{"conversation_id": "conv-1"}}
	key := conversationKey(req, ChatMeta{})
	if key != "conv-1" {
		t.Fatalf("conversation key = %q", key)
	}

	first, ok := c.pickAccount(map[string]bool{}, key, routeModel("model-x"))
	if !ok {
		t.Fatal("first pick failed")
	}
	second, ok := c.pickAccount(map[string]bool{}, key, routeModel("model-x"))
	if !ok {
		t.Fatal("second pick failed")
	}
	if first.ID() != second.ID() {
		t.Fatalf("a conversation was split across %s and %s", first.ID(), second.ID())
	}
	// A different conversation still gets its own account: stickiness must not
	// collapse the whole pool onto one credential.
	other, ok := c.pickAccount(map[string]bool{}, "conv-2", routeModel("model-x"))
	if !ok {
		t.Fatal("second conversation pick failed")
	}
	if other.ID() == first.ID() {
		t.Fatalf("a second conversation reused %s instead of rotating", other.ID())
	}
	clk.advance(time.Second)
}

// TestWorkbuddyStickyConversationYieldsToAModelPark is the fallback rule: the
// bound account is parked for the model being requested, so the request moves
// to another account and the binding is re-pointed -- it must never keep
// retrying the parked credential.
func TestWorkbuddyStickyConversationYieldsToAModelPark(t *testing.T) {
	a, b := pickAuths()
	p, _ := newPickPool([]*Auth{a, b})
	c := &Client{pool: p, affinity: core.NewAffinity(0)}
	key := "conv-1"

	first, ok := c.pickAccount(map[string]bool{}, key, routeModel("model-x"))
	if !ok {
		t.Fatal("first pick failed")
	}
	if id, ok := c.ConversationAccount(key, "model-x"); !ok || id != first.ID() {
		t.Fatalf("binding = %q/%v, want %s", id, ok, first.ID())
	}

	p.MarkModelBlocked(first, "model-x", ModelBlockReason)

	if id, ok := c.ConversationAccount(key, "model-x"); ok {
		t.Fatalf("stickiness served the parked account %s", id)
	}
	got, ok := c.pickAccount(map[string]bool{}, key, routeModel("model-x"))
	if !ok {
		t.Fatal("pick after a park failed")
	}
	if got.ID() == first.ID() {
		t.Fatalf("pick returned the parked account %s", got.ID())
	}
	if got.ID() != b.ID() {
		t.Fatalf("pick = %s, want the healthy %s", got.ID(), b.ID())
	}
	// The conversation must now be pinned to the account that actually served
	// it, so the next request does not bounce back to the parked one.
	if id, ok := c.ConversationAccount(key, "model-x"); !ok || id != got.ID() {
		t.Fatalf("rebound = %q/%v, want %s", id, ok, got.ID())
	}
}

// TestWorkbuddyConversationKeyPrefersOptionsOverMeta: the gateway supplies the
// conversation id in Options; the locally minted message id is only a fallback.
func TestWorkbuddyConversationKeyPrefersOptionsOverMeta(t *testing.T) {
	req := &core.ChatRequest{Options: map[string]any{"conversationId": "from-options"}}
	if got := conversationKey(req, ChatMeta{ConversationID: "from-meta"}); got != "from-options" {
		t.Fatalf("key = %q, want from-options", got)
	}
	req = &core.ChatRequest{}
	if got := conversationKey(req, ChatMeta{ConversationID: "from-meta"}); got != "from-meta" {
		t.Fatalf("key = %q, want from-meta", got)
	}
	if got := conversationKey(&core.ChatRequest{}, ChatMeta{}); got != "" {
		t.Fatalf("key = %q, want empty so stickiness is simply off", got)
	}
}

// TestWorkbuddyConversationKeyFallsBackToContent: a client that sends no
// conversation id at all still gets stickiness, derived from the message content
// (the reference's 粘性会话内容回退).  Without it such a client rotates accounts
// every turn and every turn re-bills the whole prefix, because the cache the
// previous account had already warmed is unreachable.
func TestWorkbuddyConversationKeyFallsBackToContent(t *testing.T) {
	msgs := []core.Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
	}
	key := conversationKey(&core.ChatRequest{Messages: msgs}, ChatMeta{})
	if !strings.HasPrefix(key, core.DerivedKeyPrefix) {
		t.Fatalf("key = %q, want a %q-derived key", key, core.DerivedKeyPrefix)
	}

	// The same conversation, one turn later: the key must not move.
	grown := &core.ChatRequest{Messages: append(append([]core.Message{}, msgs...),
		core.Message{Role: "assistant", Content: "hi"},
		core.Message{Role: "user", Content: "one more"},
	)}
	if got := conversationKey(grown, ChatMeta{}); got != key {
		t.Fatalf("the key drifted across turns: %q then %q", key, got)
	}

	// Every explicit spelling still wins over the derived key.
	explicit := &core.ChatRequest{Options: map[string]any{"conversation_id": "conv-1"}, Messages: msgs}
	if got := conversationKey(explicit, ChatMeta{}); got != "conv-1" {
		t.Fatalf("explicit key = %q, want conv-1", got)
	}
	fromMeta := &core.ChatRequest{Messages: msgs}
	if got := conversationKey(fromMeta, ChatMeta{ConversationID: "from-meta"}); got != "from-meta" {
		t.Fatalf("meta key = %q, want from-meta", got)
	}

	// A different conversation gets a different key: the fallback must not
	// collapse the whole pool onto one credential.
	other := conversationKey(&core.ChatRequest{Messages: []core.Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "goodbye"},
	}}, ChatMeta{})
	if other == key {
		t.Fatalf("two conversations derived the same key %q", key)
	}

	// And with nothing to derive from, stickiness stays off as before.
	if got := conversationKey(&core.ChatRequest{}, ChatMeta{}); got != "" {
		t.Fatalf("key = %q, want empty", got)
	}
}

// TestWorkbuddyPoolPickForModelSkipsTheParkedAccountWhenTheRestAreBusy is the
// shape that produced a stream of bogus "candidate failed" rows in production:
// the one account that may serve the model is at its in-flight ceiling, and the
// only other entry the round-robin could still hand out is the one the vendor
// has just parked for that exact model.  Walking the rotation onto the parked
// account re-asks a question the vendor already answered -- and because a 6004
// park is recorded from "now", every one of those attempts slides the window
// forward again, so the account never gets to recover inside the reset it was
// promised.  A park has to mean something: no candidate is the honest answer,
// and the caller gets to hand the request to another platform.
func TestWorkbuddyPoolPickForModelSkipsTheParkedAccountWhenTheRestAreBusy(t *testing.T) {
	a, b := pickAuths()
	p, clk := newPickPool([]*Auth{a, b})
	p.SetMaxInFlight(1)
	p.MarkModelRateLimited(a, "model-x", clk.at.Add(time.Hour), time.Minute, modelRateLimitReason)
	if !p.Acquire(b) {
		t.Fatal("Acquire(b) = false below a ceiling of 1")
	}

	if got, ok := p.PickForModel(nil, "model-x"); ok {
		if got == nil {
			t.Fatal("PickForModel = ok with a nil account")
		}
		t.Fatalf("PickForModel = %s, want no candidate: the only free account is parked for that model", got.ID())
	}

	// Releasing the other account makes room again: the park is about the
	// model, not about the pool.
	p.Release(b)
	if got, ok := p.PickForModel(nil, "model-x"); !ok || got == nil || got.ID() != b.ID() {
		t.Fatalf("PickForModel after the release = %v (ok=%v), want %s", got, ok, b.ID())
	}
}
