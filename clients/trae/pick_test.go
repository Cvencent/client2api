package trae

import (
	"testing"
	"time"

	"client2api/internal/core"
)

// pickClock is an injectable clock for the pool; pacing and TTL behaviour
// cannot be tested without moving time, and the tests must not sleep.
type pickClock struct{ at time.Time }

func (c *pickClock) now() time.Time          { return c.at }
func (c *pickClock) advance(d time.Duration) { c.at = c.at.Add(d) }

// newPickPool builds a pool whose clock the caller controls.
func newPickPool(accounts []*Auth) (*Pool, *pickClock) {
	clk := &pickClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	p := NewPool(accounts)
	p.now = clk.now
	return p, clk
}

// --- per-model cooldown ----------------------------------------------------

// TestTraePoolParksOnlyTheParkedModel: losing one model on one credential must
// not take that credential out of rotation for every other model.
func TestTraePoolParksOnlyTheParkedModel(t *testing.T) {
	a, b := testAuth("A", "ta"), testAuth("B", "tb")
	p, clk := newPickPool([]*Auth{a, b})

	if got := p.MarkModelBlocked(a, "model-x", modelBlockReason); got != modelBlockBaseTTL {
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
	clk.advance(modelBlockBaseTTL)
	if !p.UsableForModel(a.ID(), "model-x") {
		t.Fatal("the park must lapse once its deadline passes")
	}
}

// TestTraeModelBlockBackoffGrowsToTheCap pins the reference's geometric policy
// (base 6h, doubling, capped at 24h).
func TestTraeModelBlockBackoffGrowsToTheCap(t *testing.T) {
	a := testAuth("A", "ta")
	p, _ := newPickPool([]*Auth{a})
	want := []time.Duration{
		modelBlockBaseTTL,
		2 * modelBlockBaseTTL,
		modelBlockMaxTTL,
		modelBlockMaxTTL,
		modelBlockMaxTTL,
	}
	for i, w := range want {
		if got := p.MarkModelBlocked(a, "model-x", modelBlockReason); got != w {
			t.Fatalf("park %d = %v, want %v", i+1, got, w)
		}
	}
}

// TestTraeModelRateLimitHonoursTheVendorReset: a dated window is honoured, and
// one further out than the policy allows is still capped.
func TestTraeModelRateLimitHonoursTheVendorReset(t *testing.T) {
	a := testAuth("A", "ta")
	p, clk := newPickPool([]*Auth{a})

	if got := p.MarkModelRateLimited(a, "model-x", clk.at.Add(10*time.Minute), time.Minute, modelRateLimitReason); got != 10*time.Minute {
		t.Fatalf("park = %v, want the vendor's 10m window", got)
	}
	if !p.UsableForModel(a.ID(), "model-y") {
		t.Fatal("a rate-limit park must be scoped to its model")
	}
	clk.advance(time.Minute)
	far := clk.at.Add(48 * time.Hour)
	if got := p.MarkModelRateLimited(a, "model-z", far, time.Minute, modelRateLimitReason); got != softModelMax {
		t.Fatalf("park = %v, want the %v cap", got, softModelMax)
	}
}

// TestTraeClearModelCooldownOnlyClearsBackoffParks: a success retires the
// "model unavailable" backoff, but not a window the vendor dated.
func TestTraeClearModelCooldownOnlyClearsBackoffParks(t *testing.T) {
	a := testAuth("A", "ta")
	p, clk := newPickPool([]*Auth{a})

	p.MarkModelBlocked(a, "blocked", modelBlockReason)
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

// TestTraeModelFailureDelegatesToTheAccountPolicy documents the deliberate
// departure from the reference: trae's error taxonomy has no model-scoped code
// (docs/upstream/trae.md), so every classified failure keeps the account-level
// policy, and no model park is invented for it.
func TestTraeModelFailureDelegatesToTheAccountPolicy(t *testing.T) {
	a := testAuth("A", "ta")
	p, _ := newPickPool([]*Auth{a})

	kind, d := p.MarkFailureForModel(a, "model-x", &Error{Kind: ErrQuota, Status: 200})
	if kind != ErrQuota || d != quotaCooldown {
		t.Fatalf("quota = (%v, %v), want (%v, %v)", kind, d, ErrQuota, quotaCooldown)
	}
	if e := p.findLocked(a); e == nil || len(e.modelCool) != 0 {
		t.Fatal("trae has no model-scoped signal, so no model park may appear")
	}
	// A park on the whole credential subsumes any model park that does exist.
	p.MarkModelBlocked(a, "model-x", modelBlockReason)
	if _, d := p.MarkFailureForModel(a, "model-x", &Error{Kind: ErrPlanLimit, Status: 200}); d != planLimitCooldown {
		t.Fatalf("plan limit park = %v, want %v", d, planLimitCooldown)
	}
	if e := p.findLocked(a); e == nil || len(e.modelCool) != 0 {
		t.Fatal("an account park must clear the model parks it subsumes")
	}
}

// TestTraePoolPickForModelFallsBackRatherThanFailing: a model park is an
// optimisation, never a way to refuse a request.
func TestTraePoolPickForModelFallsBackRatherThanFailing(t *testing.T) {
	a, b := testAuth("A", "ta"), testAuth("B", "tb")
	p, _ := newPickPool([]*Auth{a, b})
	p.MarkModelBlocked(a, "model-x", modelBlockReason)
	p.MarkModelBlocked(b, "model-x", modelBlockReason)
	got, ok := p.PickForModel(nil, "model-x")
	if !ok || got == nil {
		t.Fatal("a fully parked model must fall back to the model-agnostic pick")
	}
	// The model-agnostic pick is untouched by the park, and so is Pick.
	if _, ok := p.Pick(nil); !ok {
		t.Fatal("the plain pick must ignore model parks")
	}
}

// --- scored selection ------------------------------------------------------

// TestTraePoolWeightedPickFavoursTheCreditHeavyAccount is the behaviour a plain
// round-robin cannot produce: with one account holding the credits and the
// other nearly empty, the rotation is visibly skewed.
func TestTraePoolWeightedPickFavoursTheCreditHeavyAccount(t *testing.T) {
	a, b := testAuth("A", "ta"), testAuth("B", "tb")
	a.SetCredits(1000, 0, "credits")
	b.SetCredits(1, 0, "credits")
	p, clk := newPickPool([]*Auth{a, b})

	countA, countB := 0, 0
	for i := 0; i < 20; i++ {
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
	if countA <= countB {
		t.Fatalf("credits did not steer the rotation: a=%d b=%d", countA, countB)
	}
	if countB < 1 {
		t.Fatalf("the thin account must never be starved entirely: a=%d b=%d", countA, countB)
	}
}

// TestTraePoolWeightedPickFavoursTheHealthierAccount: the failure penalty pulls
// a tainted account's share down as well.
func TestTraePoolWeightedPickFavoursTheHealthierAccount(t *testing.T) {
	a, b := testAuth("A", "ta"), testAuth("B", "tb")
	p, clk := newPickPool([]*Auth{a, b})

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
	ea.fails = 4 // floored at minWeight
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

// TestTraePoolMinPickGapSpreadsConcurrentPicks: two requests in the same
// instant must not both land on the credential the other just used.
func TestTraePoolMinPickGapSpreadsConcurrentPicks(t *testing.T) {
	a, b, c := testAuth("A", "ta"), testAuth("B", "tb"), testAuth("C", "tc")
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

// TestTraePoolSnapshotRendersAParkedModel keeps the parked models observable:
// a park nobody can see is a park nobody can debug.
func TestTraePoolSnapshotRendersAParkedModel(t *testing.T) {
	a := testAuth("A", "ta")
	p, _ := newPickPool([]*Auth{a})
	p.MarkModelBlocked(a, "model-x", modelBlockReason)
	snap := p.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d", len(snap))
	}
	parks, ok := snap[0].Extra["model_cooldowns"].([]core.ModelPark)
	if !ok || len(parks) != 1 {
		t.Fatalf("model_cooldowns = %#v, want one []core.ModelPark entry", snap[0].Extra["model_cooldowns"])
	}
	if parks[0].Model != "model-x" {
		t.Fatalf("parked model = %q, want model-x", parks[0].Model)
	}
	if parks[0].Kind != core.ModelParkUnsupported {
		t.Fatalf("parked kind = %q, want %q", parks[0].Kind, core.ModelParkUnsupported)
	}
}

// --- conversation -> account affinity --------------------------------------

// TestTraeConversationSticksToItsAccount: a mid-conversation failover is
// expensive against trae's 12h/6h account parks, so the conversation keeps its
// account even though the scored pick would have rotated.
func TestTraeConversationSticksToItsAccount(t *testing.T) {
	a, b := testAuth("A", "ta"), testAuth("B", "tb")
	p, clk := newPickPool([]*Auth{a, b})
	c := testClient(t, nil, []*Auth{a, b}, nil)
	c.pool = p
	c.affinity = core.NewAffinity(0)

	req := &core.ChatRequest{User: "user-1", Options: map[string]any{"conversation_id": "conv-1"}}
	key := conversationKey(req)
	if key != "conv-1" {
		t.Fatalf("conversation key = %q", key)
	}

	first, ok := c.pickAccount(map[string]bool{}, key, "model-x")
	if !ok {
		t.Fatal("first pick failed")
	}
	second, ok := c.pickAccount(map[string]bool{}, key, "model-x")
	if !ok {
		t.Fatal("second pick failed")
	}
	if first.ID() != second.ID() {
		t.Fatalf("a conversation was split across %s and %s", first.ID(), second.ID())
	}
	other, ok := c.pickAccount(map[string]bool{}, "conv-2", "model-x")
	if !ok {
		t.Fatal("second conversation pick failed")
	}
	if other.ID() == first.ID() {
		t.Fatalf("a second conversation reused %s instead of rotating", other.ID())
	}
	clk.advance(time.Second)
}

// TestTraeStickyConversationYieldsToAPark is the fallback rule: the bound
// account is parked for the requested model, so the request moves and the
// binding is re-pointed -- stickiness never keeps retrying a parked account.
func TestTraeStickyConversationYieldsToAPark(t *testing.T) {
	a, b := testAuth("A", "ta"), testAuth("B", "tb")
	p, _ := newPickPool([]*Auth{a, b})
	c := testClient(t, nil, []*Auth{a, b}, nil)
	c.pool = p
	c.affinity = core.NewAffinity(0)
	key := "conv-1"

	first, ok := c.pickAccount(map[string]bool{}, key, "model-x")
	if !ok {
		t.Fatal("first pick failed")
	}
	if id, ok := c.ConversationAccount(key, "model-x"); !ok || id != first.ID() {
		t.Fatalf("binding = %q/%v, want %s", id, ok, first.ID())
	}

	p.MarkModelBlocked(first, "model-x", modelBlockReason)

	if id, ok := c.ConversationAccount(key, "model-x"); ok {
		t.Fatalf("stickiness served the parked account %s", id)
	}
	got, ok := c.pickAccount(map[string]bool{}, key, "model-x")
	if !ok {
		t.Fatal("pick after a park failed")
	}
	if got.ID() != b.ID() {
		t.Fatalf("pick = %s, want the healthy %s", got.ID(), b.ID())
	}
	if id, ok := c.ConversationAccount(key, "model-x"); !ok || id != got.ID() {
		t.Fatalf("rebound = %q/%v, want %s", id, ok, got.ID())
	}
}

// TestTraeConversationKeyComesFromOptions: the gateway supplies the id in
// Options; a caller that supplies nothing gets no stickiness rather than a
// wrong one.
func TestTraeConversationKeyComesFromOptions(t *testing.T) {
	req := &core.ChatRequest{Options: map[string]any{"conversation_id": "from-options"}}
	if got := conversationKey(req); got != "from-options" {
		t.Fatalf("key = %q, want from-options", got)
	}
	if got := conversationKey(&core.ChatRequest{}); got != "" {
		t.Fatalf("key = %q, want empty so stickiness is simply off", got)
	}
}
