package workbuddy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// The in-flight ceiling is the one mechanism in this module whose absence is
// invisible until the vendor's WAF answers: without it the gateway happily opens
// as many concurrent requests as the caller sends.  These tests pin the ceiling
// itself (what Acquire admits), the picking rule (a full account is skipped
// rather than queued), and the request path that turns "nowhere to send it" into
// backpressure instead of a burst.

// leaseAuth builds a credential the pool can hold.  Only the id and the realm
// matter for in-flight accounting, so the rest stays obviously fake.
func leaseAuth(uid, realm string) *Auth {
	return &Auth{AccessToken: "at-" + uid + "-0123456789", UID: uid, Realm: realm}
}

// TestWorkbuddyInFlightCeilingRefusesTheExtraSlot is the core rule: a slot per
// account, and the extra request is refused rather than queued.
func TestWorkbuddyInFlightCeilingRefusesTheExtraSlot(t *testing.T) {
	a := leaseAuth("u-1", "")
	b := leaseAuth("u-2", "")
	p, _ := newPickPool([]*Auth{a, b})
	p.SetMaxInFlight(1)

	if perAccount, global := p.Limits(); perAccount != 1 || global != 0 {
		t.Fatalf("Limits = (%d,%d), want (1,0)", perAccount, global)
	}
	if !p.Acquire(a) {
		t.Fatal("first Acquire(a) = false, want true below the ceiling")
	}
	if p.Acquire(a) {
		t.Fatal("second Acquire(a) = true, want false at the ceiling")
	}
	if total, full := p.InFlight(); total != 1 || full != 1 {
		t.Fatalf("InFlight = (%d,%d), want (1,1)", total, full)
	}
	if p.AllFull() {
		t.Fatal("AllFull = true while b still has a free slot")
	}
	if !p.Acquire(b) {
		t.Fatal("Acquire(b) = false while b is free")
	}
	if !p.AllFull() {
		t.Fatal("AllFull = false with both accounts at their ceiling")
	}

	p.Release(a)
	if total, _ := p.InFlight(); total != 1 {
		t.Fatalf("after Release(a) total = %d, want 1", total)
	}
	p.Release(a) // a response body closed twice must not raise the real ceiling
	if total, _ := p.InFlight(); total != 1 {
		t.Fatalf("a second Release(a) changed the count: total = %d, want 1", total)
	}
	p.Release(b)
	if total, full := p.InFlight(); total != 0 || full != 0 {
		t.Fatalf("InFlight = (%d,%d) after every slot was returned, want (0,0)", total, full)
	}
	if p.AllFull() {
		t.Fatal("AllFull = true after every slot was returned")
	}
}

// TestWorkbuddyInFlightZeroCeilingNeverRefuses guards the "0 = unlimited"
// semantics: an operator who never configured a ceiling must keep exactly the
// behaviour the module had before ceilings existed.
func TestWorkbuddyInFlightZeroCeilingNeverRefuses(t *testing.T) {
	a := leaseAuth("u-1", "")
	b := leaseAuth("u-2", "")
	p, _ := newPickPool([]*Auth{a, b})
	p.SetMaxInFlight(0)

	for i := 1; i <= 5; i++ {
		if !p.Acquire(a) {
			t.Fatalf("Acquire(a) #%d = false with no ceiling", i)
		}
	}
	if total, full := p.InFlight(); total != 5 || full != 0 {
		t.Fatalf("InFlight = (%d,%d), want (5,0) with no ceiling", total, full)
	}
	if p.AllFull() {
		t.Fatal("AllFull = true with no ceiling")
	}
	if got, ok := p.Find(nil, a.ID()); !ok || got == nil {
		t.Fatal("Find = not ok for an account with no ceiling")
	}
	if got, ok := p.Pick(nil); !ok || got == nil {
		t.Fatal("Pick = not ok while a free slot exists")
	}
}

// TestWorkbuddyInFlightGlobalTierIsTighterThanThePlainCeiling pins the reason
// the second ceiling exists: the realm that produced the WAF 403s gets a
// smaller limit than everyone else, and "unset" (0) must fall back rather than
// mean "allow nothing".
func TestWorkbuddyInFlightGlobalTierIsTighterThanThePlainCeiling(t *testing.T) {
	g := leaseAuth("u-global", "global")
	if !g.IsGlobal() {
		t.Fatalf("fixture Realm = %q is not recognised as global", g.Realm)
	}
	cn := leaseAuth("u-cn", "")
	p, _ := newPickPool([]*Auth{g, cn})
	p.SetMaxInFlight(3)
	p.SetMaxInFlightGlobal(2)

	for i := 1; i <= 2; i++ {
		if !p.Acquire(g) {
			t.Fatalf("Acquire(global) #%d = false below the global tier of 2", i)
		}
	}
	if p.Acquire(g) {
		t.Fatal("Acquire(global) = true at the global tier of 2")
	}
	for i := 1; i <= 3; i++ {
		if !p.Acquire(cn) {
			t.Fatalf("Acquire(cn) #%d = false below the plain ceiling of 3", i)
		}
	}
	if p.Acquire(cn) {
		t.Fatal("Acquire(cn) = true past the plain ceiling of 3")
	}

	// 0 means "the configuration said nothing": the tier must fall back to the
	// plain ceiling instead of freezing the realm out entirely.
	p.SetMaxInFlightGlobal(0)
	p.Release(g)
	p.Release(g)
	for i := 1; i <= 3; i++ {
		if !p.Acquire(g) {
			t.Fatalf("Acquire(global) #%d = false after the tier was cleared to 0", i)
		}
	}
}

// TestWorkbuddyFullAccountsAreSkippedWhenPicking is the backpressure rule: an
// account at its ceiling is not a candidate, and "at a ceiling" must stay
// distinguishable from "unhealthy".
func TestWorkbuddyFullAccountsAreSkippedWhenPicking(t *testing.T) {
	a := leaseAuth("u-1", "")
	b := leaseAuth("u-2", "")
	p, _ := newPickPool([]*Auth{a, b})
	p.SetMaxInFlight(1)

	if !p.Acquire(a) {
		t.Fatal("Acquire(a) = false below the ceiling")
	}
	if p.AllFull() {
		t.Fatal("AllFull = true while b is free")
	}

	got, ok := p.Pick(nil)
	if !ok || got == nil || got.ID() != b.ID() {
		t.Fatalf("Pick = %v (ok=%v), want the free account %s", got, ok, b.ID())
	}
	if _, ok := p.Find(nil, a.ID()); ok {
		t.Fatal("Find(a) = ok although a is at its ceiling")
	}
	if _, ok := p.Find(nil, b.ID()); !ok {
		t.Fatal("Find(b) = not ok although b is free")
	}
	if got, ok := p.PickForModel(nil, "claude-sonnet-4"); !ok || got == nil || got.ID() != b.ID() {
		t.Fatalf("PickForModel = %v (ok=%v), want the free account %s", got, ok, b.ID())
	}

	if !p.Acquire(b) {
		t.Fatal("Acquire(b) = false below the ceiling")
	}
	if _, ok := p.Pick(nil); ok {
		t.Fatal("Pick = ok with every account at its ceiling")
	}
	if _, ok := p.PickForModel(nil, "claude-sonnet-4"); ok {
		t.Fatal("PickForModel = ok with every account at its ceiling")
	}
	if _, ok := p.Find(nil, b.ID()); ok {
		t.Fatal("Find(b) = ok although b is at its ceiling")
	}
	if !p.AllFull() {
		t.Fatal("AllFull = false with every account at its ceiling")
	}
	// Health and occupancy are different questions: the pool is still ready.
	if !p.Ready() {
		t.Fatal("Ready = false; being at a ceiling is not being unhealthy")
	}
}

// chatRequest is the smallest request the request path accepts.
func chatRequest() *core.ChatRequest {
	return &core.ChatRequest{
		Model:    "claude-sonnet-4",
		User:     "conv-1",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}
}

// TestWorkbuddyChatRefusesWhenEveryAccountIsAtItsCeiling is the end-to-end
// version of the rule: with no free slot the module reports backpressure
// (core.ErrBusy, which the gateway renders as 429 + Retry-After) and makes no
// upstream call at all — a burst must not be produced while refusing it.
func TestWorkbuddyChatRefusesWhenEveryAccountIsAtItsCeiling(t *testing.T) {
	rt := &fakeRT{}
	c, _ := newTestClient(t, rt)
	if got := c.pool.Len(); got != 1 {
		t.Fatalf("the fixture holds %d account(s), this test assumes exactly 1", got)
	}
	auth := c.findAuthByUID("uid-test-0001")
	if auth == nil {
		t.Fatal("the fixture holds no known account")
	}
	c.pool.SetMaxInFlight(1)
	if !c.pool.Acquire(auth) {
		t.Fatal("Acquire = false for the fixture account below a ceiling of 1")
	}
	if !c.pool.AllFull() {
		t.Fatal("AllFull = false with the only account at its ceiling")
	}

	_, err := c.Chat(context.Background(), chatRequest())
	if !errors.Is(err, core.ErrBusy) {
		t.Fatalf("Chat = %v, want core.ErrBusy while every account is at its ceiling", err)
	}
	if got := len(rt.calls); got != 0 {
		t.Fatalf("Chat made %d upstream call(s) while refusing for backpressure", got)
	}
}

// TestWorkbuddyChatHoldsTheSlotForAsLongAsTheStreamIsOpen pins the other half
// of the contract: the slot is held for the life of the response body and
// returned exactly once when it is closed.
func TestWorkbuddyChatHoldsTheSlotForAsLongAsTheStreamIsOpen(t *testing.T) {
	rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
		return sseResponse(200, sseFixture, nil), nil
	}}
	c, _ := newTestClient(t, rt)
	c.pool.SetMaxInFlight(1)

	stream, err := c.Chat(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if total, full := c.pool.InFlight(); total != 1 || full != 1 {
		t.Fatalf("InFlight = (%d,%d) while the stream is open, want (1,1)", total, full)
	}
	if _, ok := c.pool.Pick(nil); ok {
		t.Fatal("Pick = ok while the only account is holding its slot")
	}

	_ = stream.Close()
	if total, full := c.pool.InFlight(); total != 0 || full != 0 {
		t.Fatalf("InFlight = (%d,%d) after Close, want (0,0)", total, full)
	}
	_ = stream.Close() // a caller that closes twice must not break the count
	if total, _ := c.pool.InFlight(); total != 0 {
		t.Fatalf("a second Close changed the count: total = %d, want 0", total)
	}
}

// TestWorkbuddyChatReturnsTheSlotWhenTheAttemptFails covers the leak that would
// silently raise the real ceiling: a failed attempt must give its slot back
// before the next account is tried.
func TestWorkbuddyChatReturnsTheSlotWhenTheAttemptFails(t *testing.T) {
	rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(500, `{"code":500,"msg":"boom"}`), nil
	}}
	c, _ := newTestClient(t, rt)
	c.pool.SetMaxInFlight(1)

	if _, err := c.Chat(context.Background(), chatRequest()); err == nil {
		t.Fatal("Chat = nil error on an upstream 500")
	}
	if total, _ := c.pool.InFlight(); total != 0 {
		t.Fatalf("InFlight = %d after a failed attempt, want 0 (the slot leaked)", total)
	}
}

// TestWorkbuddyLeasedStreamReleasesExactlyOnce tests the wrapper in isolation,
// so a regression there cannot hide behind a successful request.
func TestWorkbuddyLeasedStreamReleasesExactlyOnce(t *testing.T) {
	released := 0
	st := newLeasedStream(context.Background(), io.NopCloser(strings.NewReader("x")), func() {
		released++
	})
	_ = st.Close()
	_ = st.Close()
	if released != 1 {
		t.Fatalf("release ran %d times, want exactly 1", released)
	}
}

// TestWorkbuddyHealthIsStricterThanEveryAccountBeingHealthy pins the rule the
// reference added after the WAF 403s: a pool whose healthy accounts are all at
// their ceiling cannot serve, and saying "healthy" would only send the caller
// back into the same wall.
func TestWorkbuddyHealthIsStricterThanEveryAccountBeingHealthy(t *testing.T) {
	empty, _ := newPickPool(nil)
	if h := (&Client{pool: empty}).Health(); h.Servable {
		t.Fatalf("Health = %+v, want not servable with no account", h)
	} else if h.Note == "" {
		t.Fatal("Health has no note explaining why it cannot serve")
	}

	a := leaseAuth("u-1", "")
	p, _ := newPickPool([]*Auth{a})
	p.SetMaxInFlight(1)
	c := &Client{pool: p}

	h := c.Health()
	if !h.Servable {
		t.Fatalf("Health = %+v, want servable with one free account", h)
	}
	if h.Ready != 1 || h.Total != 1 || h.Cooling != 0 || h.Disabled != 0 {
		t.Fatalf("Health census = %+v, want ready=1 total=1 cooling=0 disabled=0", h)
	}

	if !p.Acquire(a) {
		t.Fatal("Acquire(a) = false below the ceiling")
	}
	h = c.Health()
	if h.Servable {
		t.Fatalf("Health = %+v, want not servable while the only usable account is at its ceiling", h)
	}
	if h.Ready != 1 {
		t.Fatalf("Health.Ready = %d, want 1: a busy account is still healthy", h.Ready)
	}
	if !strings.Contains(h.Note, "in-flight") {
		t.Fatalf("Health.Note = %q, want it to name the in-flight limit", h.Note)
	}
}

// TestWorkbuddyPoolStatsReportTheLiveCounters checks the panel-facing numbers
// come from the same counters the admission decision uses.
func TestWorkbuddyPoolStatsReportTheLiveCounters(t *testing.T) {
	a := leaseAuth("u-1", "")
	b := leaseAuth("u-2", "")
	p, _ := newPickPool([]*Auth{a, b})
	p.SetMaxInFlight(1)
	c := &Client{pool: p, affinity: core.NewAffinity(core.DefaultAffinityTTL)}
	c.affinity.Bind("conv-a", a.ID())
	c.affinity.Bind("conv-b", b.ID())

	if s := c.PoolStats(); s.InFlight != 0 || s.InFlightFull != 0 || s.StickySessions != 2 {
		t.Fatalf("PoolStats = %+v, want in_flight=0 in_flight_full=0 sticky=2", s)
	}
	if !p.Acquire(a) {
		t.Fatal("Acquire(a) = false below the ceiling")
	}
	s := c.PoolStats()
	if s.InFlight != 1 || s.InFlightFull != 1 {
		t.Fatalf("PoolStats = %+v, want in_flight=1 in_flight_full=1", s)
	}
	if s.StickySessions != 2 {
		t.Fatalf("PoolStats.StickySessions = %d, want 2", s.StickySessions)
	}
}

// TestWorkbuddyChatRefusesAModelEveryAccountIsParkedFor is the end-to-end face
// of the same rule: when the pool has no account left for the requested model,
// Chat must not spend an upstream call re-asking a question the vendor already
// answered with 6004.  It reports the platform exhausted so the router can hand
// the request to another platform instead of logging a failure against an
// account that is only parked for that one model.
func TestWorkbuddyChatRefusesAModelEveryAccountIsParkedFor(t *testing.T) {
	rt := &fakeRT{}
	c, _ := newTestClient(t, rt)
	auth := c.findAuthByUID("uid-test-0001")
	if auth == nil {
		t.Fatal("the fixture holds no known account")
	}

	req := chatRequest()
	c.pool.MarkModelRateLimited(auth, strings.TrimSpace(req.Model), time.Now().Add(2*time.Hour), time.Minute, modelRateLimitReason)

	_, err := c.Chat(context.Background(), req)
	if !errors.Is(err, core.ErrPlatformExhausted) {
		t.Fatalf("Chat = %v, want core.ErrPlatformExhausted for a model parked on every account", err)
	}
	if got := len(rt.calls); got != 0 {
		t.Fatalf("Chat made %d upstream call(s) for a model the vendor already parked", got)
	}
}
