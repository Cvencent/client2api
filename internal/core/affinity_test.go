package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock lets the tests move time without sleeping.
type affinityClock struct {
	mu sync.Mutex
	t  time.Time
}

func newAffinityClock() *affinityClock {
	return &affinityClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *affinityClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *affinityClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testAffinity is an Affinity with an injectable clock, which is the only way
// the TTL and GC behaviour can be observed without sleeping for 30 minutes.
func testAffinity(t *testing.T, ttl time.Duration) (*Affinity, *affinityClock) {
	t.Helper()
	clk := newAffinityClock()
	a := NewAffinity(ttl)
	a.now = clk.now
	return a, clk
}

func TestAffinityDefaultsToTheReferenceTTL(t *testing.T) {
	if got := NewAffinity(0).TTL(); got != DefaultAffinityTTL {
		t.Fatalf("zero ttl = %v, want %v", got, DefaultAffinityTTL)
	}
	if got := NewAffinity(-time.Minute).TTL(); got != DefaultAffinityTTL {
		t.Fatalf("negative ttl = %v, want %v", got, DefaultAffinityTTL)
	}
	if DefaultAffinityTTL != 30*time.Minute || DefaultAffinityGCInterval != 5*time.Minute {
		t.Fatalf("defaults drifted: ttl=%v gc=%v", DefaultAffinityTTL, DefaultAffinityGCInterval)
	}
}

func TestAffinityBindsAndResolves(t *testing.T) {
	a, _ := testAffinity(t, 0)
	if _, ok := a.Resolve("conv-1", nil); ok {
		t.Fatal("an unbound key must not resolve")
	}
	a.Bind("conv-1", "acct-a")
	got, ok := a.Resolve("conv-1", nil)
	if !ok || got != "acct-a" {
		t.Fatalf("resolve = %q/%v, want acct-a/true", got, ok)
	}
	if a.Count() != 1 {
		t.Fatalf("count = %d, want 1", a.Count())
	}
	// Re-binding is an overwrite, not a second entry.
	a.Bind("conv-1", "acct-b")
	if got, _ := a.Resolve("conv-1", nil); got != "acct-b" {
		t.Fatalf("rebind = %q, want acct-b", got)
	}
	if a.Count() != 1 {
		t.Fatalf("count after rebind = %d, want 1", a.Count())
	}
	if !a.Unbind("conv-1") {
		t.Fatal("unbind of a bound key must report true")
	}
	if a.Unbind("conv-1") {
		t.Fatal("unbind of an unbound key must report false")
	}
	if _, ok := a.Resolve("conv-1", nil); ok {
		t.Fatal("an unbound key must not resolve")
	}
}

func TestAffinityIgnoresEmptyKeys(t *testing.T) {
	a, _ := testAffinity(t, 0)
	a.Bind("", "acct-a")
	a.Bind("conv-1", "")
	if a.Count() != 0 {
		t.Fatalf("count = %d, want 0", a.Count())
	}
	if _, ok := a.Resolve("", nil); ok {
		t.Fatal("the empty key must never resolve")
	}
}

// TestAffinityDropsABindingToAnUnusableAccount is the fallback rule the cost
// argument depends on: stickiness must never serve a parked account, and it
// must never keep a dead binding around to be retried on the next request.
func TestAffinityDropsABindingToAnUnusableAccount(t *testing.T) {
	a, _ := testAffinity(t, 0)
	a.Bind("conv-1", "acct-parked")

	got, ok := a.Resolve("conv-1", func(id string) bool { return id == "acct-live" })
	if ok {
		t.Fatalf("resolve returned the parked account %q", got)
	}
	if a.Count() != 0 {
		t.Fatalf("the dead binding was kept: count = %d", a.Count())
	}
	// The caller now picks normally and re-binds; the next request sticks to it.
	a.Bind("conv-1", "acct-live")
	if got, ok := a.Resolve("conv-1", func(id string) bool { return id == "acct-live" }); !ok || got != "acct-live" {
		t.Fatalf("relbound resolve = %q/%v", got, ok)
	}
}

// TestAffinityDisabledIsNotAWindowOfZero pins the difference between the two
// session_sticky knobs.  The reference does not implement "off" by shortening
// the window: it builds no session router at all, so every request selects an
// account from scratch.  Disabling must therefore also drop the bindings -- a
// table that kept them would resurrect a routing decision made under a policy
// that is no longer in force the moment the operator turned the switch back on,
// and the reference, never having built the router, has no such memory either.
func TestAffinityDisabledIsNotAWindowOfZero(t *testing.T) {
	a, _ := testAffinity(t, 0)
	if !a.Enabled() {
		t.Fatal("stickiness must start enabled, like the reference's default")
	}
	a.Bind("conv-1", "acct-a")
	if a.Count() != 1 {
		t.Fatalf("count = %d, want 1", a.Count())
	}

	a.SetEnabled(false)
	if a.Enabled() {
		t.Fatal("SetEnabled(false) left the table enabled")
	}
	if a.Count() != 0 {
		t.Fatalf("disabling kept %d binding(s)", a.Count())
	}
	if got, ok := a.Resolve("conv-1", nil); ok {
		t.Fatalf("a disabled table resolved %q", got)
	}
	// A bind while disabled must not silently accumulate either.
	a.Bind("conv-2", "acct-b")
	if a.Count() != 0 {
		t.Fatalf("bind while disabled stored %d entry/entries", a.Count())
	}

	// The switch is not the window: the TTL is untouched, so turning it back on
	// restores the feature with the window it already had.
	if got := a.TTL(); got != DefaultAffinityTTL {
		t.Fatalf("ttl = %v, want %v", got, DefaultAffinityTTL)
	}
	a.SetEnabled(true)
	if !a.Enabled() {
		t.Fatal("SetEnabled(true) did not re-enable the table")
	}
	a.Bind("conv-3", "acct-c")
	if got, ok := a.Resolve("conv-3", nil); !ok || got != "acct-c" {
		t.Fatalf("resolve after re-enable = %q/%v, want acct-c/true", got, ok)
	}
}

// TestAffinityZeroValueStaysEnabled guards the inverted flag.  session_sticky
// defaults to on when the file is silent, so "on" has to be the zero value: an
// Affinity reached through a struct literal rather than NewAffinity must still
// start in the documented state instead of silently losing stickiness.
func TestAffinityZeroValueStaysEnabled(t *testing.T) {
	var zero Affinity
	if !zero.Enabled() {
		t.Fatal("the zero Affinity must report enabled")
	}
	var nilHolder *Affinity
	if nilHolder.Enabled() {
		t.Fatal("a nil Affinity must report disabled")
	}
	nilHolder.SetEnabled(false) // must not panic
}

func TestAffinityTTLIsIdleAndGCSweepsExpiredBindings(t *testing.T) {
	a, clk := testAffinity(t, 30*time.Minute)
	for i := 0; i < 100; i++ {
		a.Bind(fmt.Sprintf("conv-%d", i), "acct-a")
	}
	if a.Count() != 100 {
		t.Fatalf("count = %d, want 100", a.Count())
	}

	// Halfway through the TTL a resolve refreshes only the key it touched.
	clk.advance(20 * time.Minute)
	if _, ok := a.Resolve("conv-0", nil); !ok {
		t.Fatal("a binding inside the TTL must resolve")
	}
	clk.advance(20 * time.Minute) // conv-0 is 20m idle; the rest are 40m idle

	if _, ok := a.Resolve("conv-0", nil); !ok {
		t.Fatal("resolving must refresh the idle timer")
	}
	if _, ok := a.Resolve("conv-1", nil); ok {
		t.Fatal("a binding past the TTL must not resolve")
	}
	if _, ok := a.Resolve("conv-1", nil); ok {
		t.Fatal("dropping an expired binding must be durable")
	}

	// The lazy path cleaned the ones it was asked about; GC is what keeps the
	// table from growing with keys nobody asks about any more.
	removed := a.GC()
	if removed != 98 {
		t.Fatalf("GC removed %d, want 98", removed)
	}
	if a.Count() != 1 {
		t.Fatalf("count after GC = %d, want 1", a.Count())
	}
	if removed := a.GC(); removed != 0 {
		t.Fatalf("a second GC removed %d, want 0", removed)
	}
}

func TestAffinityExpiredBindingIsNotResolvableEvenWithNoPredicate(t *testing.T) {
	a, clk := testAffinity(t, time.Minute)
	a.Bind("conv-1", "acct-a")
	clk.advance(2 * time.Minute)
	if _, ok := a.Resolve("conv-1", nil); ok {
		t.Fatal("an expired binding must not resolve")
	}
	if a.Count() != 0 {
		t.Fatalf("the expired binding was kept: count = %d", a.Count())
	}
}

// TestAffinityNilHolderIsSafe is the property that makes the mechanism opt-in:
// a module can hold an *Affinity that was never built and call it freely.
// The stickiness window and the sweep cadence are operator-settable now:
// session_sticky.* in the config used to be parsed, defaulted and logged while
// nothing read it.
func TestAffinitySetTTLAndGCInterval(t *testing.T) {
	var nilTable *Affinity
	nilTable.SetTTL(time.Minute) // must not panic
	nilTable.SetGCInterval(time.Minute)
	if got := nilTable.GCInterval(); got != 0 {
		t.Fatalf("a nil table reports a sweep interval of %s", got)
	}

	a, clk := testAffinity(t, time.Hour)
	a.SetTTL(2 * time.Minute)
	if got := a.TTL(); got != 2*time.Minute {
		t.Fatalf("TTL = %s after SetTTL, want 2m", got)
	}

	// The live TTL decides expiry, so shortening the window takes effect on
	// bindings that are already written rather than only on new ones.
	a.Bind("conv-1", "uid-1")
	clk.advance(3 * time.Minute)
	if _, ok := a.Resolve("conv-1", nil); ok {
		t.Fatal("a binding outlived the window the operator shortened")
	}

	// A blank value selects the documented default rather than a window of
	// zero, which would look like "stickiness is off" to whoever cleared it.
	a.SetTTL(0)
	if got := a.TTL(); got != DefaultAffinityTTL {
		t.Fatalf("TTL = %s after SetTTL(0), want %s", got, DefaultAffinityTTL)
	}

	if got := a.GCInterval(); got != DefaultAffinityGCInterval {
		t.Fatalf("sweep interval = %s by default, want %s", got, DefaultAffinityGCInterval)
	}
	a.SetGCInterval(0)
	if got := a.GCInterval(); got != DefaultAffinityGCInterval {
		t.Fatalf("sweep interval = %s after SetGCInterval(0), want %s", got, DefaultAffinityGCInterval)
	}

	// Changing the cadence while the sweep is running has to be observed too:
	// StartGC bakes the interval into its ticker at launch.
	a.StartGC()
	defer a.StopGC()
	a.SetGCInterval(15 * time.Minute)
	if got := a.GCInterval(); got != 15*time.Minute {
		t.Fatalf("sweep interval = %s after SetGCInterval on a running sweep, want 15m", got)
	}
}

func TestAffinityNilHolderIsSafe(t *testing.T) {
	var a *Affinity
	a.Bind("conv-1", "acct-a")
	if _, ok := a.Resolve("conv-1", nil); ok {
		t.Fatal("a nil table must not resolve")
	}
	if a.Unbind("conv-1") {
		t.Fatal("a nil table must not report an unbind")
	}
	if a.Count() != 0 {
		t.Fatal("a nil table must be empty")
	}
	if a.GC() != 0 {
		t.Fatal("a nil table must not report GC work")
	}
	if a.TTL() != 0 {
		t.Fatal("a nil table has no TTL")
	}
	a.StartGC()
	a.StopGC()
}

func TestAffinityStartGCIsIdempotentAndStoppable(t *testing.T) {
	a, clk := testAffinity(t, time.Minute)
	a.gcEvery = time.Millisecond
	a.Bind("conv-1", "acct-a")
	a.StartGC()
	a.StartGC() // must not launch a second sweep or panic on a double close
	a.StopGC()
	a.StopGC() // must be safe without a running sweep

	// A stopped sweep is a stopped sweep: nothing is collected behind our back.
	clk.advance(time.Hour)
	if a.Count() != 1 {
		t.Fatalf("count = %d, want the entry to survive while stopped", a.Count())
	}
}

// TestAffinityConcurrentAccess exercises the map under -race: many conversations
// resolving and binding while a GC sweep runs.
func TestAffinityConcurrentAccess(t *testing.T) {
	a := NewAffinity(time.Minute)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				a.GC()
			}
		}
	}()
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("conv-%d", (g*i)%17)
				a.Bind(key, fmt.Sprintf("acct-%d", g))
				a.Resolve(key, func(string) bool { return true })
				a.Count()
				a.Unbind(fmt.Sprintf("other-%d", i))
			}
		}(g)
	}
	close(stop)
	wg.Wait()
}

// TestConversationKeyOfPrefersTheResolvedID pins the request-level resolver:
// the gateway already knows which spelling arrived, so a module must take that
// answer rather than re-deriving it from the option map — a client that puts
// its conversation id in metadata has no option key for the old path to see.
func TestConversationKeyOfPrefersTheResolvedID(t *testing.T) {
	tests := []struct {
		name string
		req  *ChatRequest
		want string
	}{
		{"nil request", nil, ""},
		{"nothing to key on", &ChatRequest{}, ""},
		{"resolved id", &ChatRequest{ConversationID: "conv-1"}, "conv-1"},
		{"resolved id is trimmed", &ChatRequest{ConversationID: "  conv-1  "}, "conv-1"},
		{"resolved id beats the option spellings", &ChatRequest{ConversationID: "conv-1", Options: map[string]any{"conversation_id": "conv-2"}}, "conv-1"},
		{"resolved id beats the user fallback", &ChatRequest{ConversationID: "conv-1", User: "user-1"}, "conv-1"},
		{"blank resolved id falls through to options", &ChatRequest{ConversationID: "   ", Options: map[string]any{"conversation_id": "conv-2"}}, "conv-2"},
		{"blank resolved id falls through to the user", &ChatRequest{ConversationID: "   ", User: "user-1"}, "user-1"},
		{"option spellings still work", &ChatRequest{Options: map[string]any{"conversationId": "conv-3"}}, "conv-3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConversationKeyOf(tc.req); got != tc.want {
				t.Fatalf("ConversationKeyOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConversationKeyPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		options map[string]any
		user    string
		want    string
	}{
		{"snake case wins", map[string]any{"conversation_id": "c1", "conversationId": "c2"}, "", "c1"},
		{"camel case", map[string]any{"conversationId": "c2"}, "", "c2"},
		{"prompt cache key", map[string]any{"prompt_cache_key": "pck"}, "", "pck"},
		{"numeric id", map[string]any{"conversation_id": float64(123456)}, "", "123456"},
		{"json number", map[string]any{"conversation_id": json.Number("98765432109876543210")}, "", "98765432109876543210"},
		{"trimmed", map[string]any{"conversation_id": "  c3  "}, "", "c3"},
		{"blank falls through", map[string]any{"conversation_id": "   "}, "user-1", "user-1"},
		{"user fallback", nil, "user-1", "user-1"},
		{"structured values are not keys", map[string]any{"conversation_id": map[string]any{"a": 1}}, "", ""},
		{"nothing to key on", nil, "", ""},
		{"nil options", map[string]any{"conversation_id": nil}, "user-1", "user-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConversationKey(tc.options, tc.user); got != tc.want {
				t.Fatalf("ConversationKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAsConversationBinderRequiresOptIn pins the discovery contract: a client
// that does not implement the interface must report false rather than fail.
// plainClient lives in capabilities_test.go and is exactly "a module that opted
// into nothing".
func TestAsConversationBinderRequiresOptIn(t *testing.T) {
	if _, ok := AsConversationBinder(nil); ok {
		t.Fatal("a nil client must not report the capability")
	}
	if _, ok := AsConversationBinder(&plainClient{name: "plain"}); ok {
		t.Fatal("a client without the interface must report false")
	}
	if _, ok := AsConversationBinder(&affinityStub{plainClient: plainClient{name: "sticky"}}); !ok {
		t.Fatal("a client with the interface must be discovered")
	}
	// The interface must be usable through the discovered value, not merely
	// detected: a wrong method set would still satisfy the assertion above.
	var b ConversationBinder
	b, _ = AsConversationBinder(&affinityStub{plainClient: plainClient{name: "sticky"}})
	b.BindConversation("conv-1", "acct-a")
	if _, ok := b.ConversationAccount("conv-1", "model-x"); !ok {
		t.Fatal("the discovered binder did not record the binding")
	}
}

type affinityStub struct {
	plainClient
	mu sync.Mutex
	m  map[string]string
}

func (c *affinityStub) BindConversation(key, account string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]string{}
	}
	c.m[key] = account
}

func (c *affinityStub) UnbindConversation(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.m[key]
	delete(c.m, key)
	return ok
}

func (c *affinityStub) ConversationAccount(key, _ string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, ok
}

// TestConversationKeyOfStaysOffForAnUnscopedRequest pins the opt-in contract the
// derived key must not break: ConversationKeyOf itself keeps answering "" for a
// request that names no conversation, so every module that pins "an unscoped
// request rotates exactly as before" still does.  The content fallback is a
// separate call a module has to make on purpose.
func TestConversationKeyOfStaysOffForAnUnscopedRequest(t *testing.T) {
	req := &ChatRequest{Messages: []Message{{Role: "user", Content: "hello"}}}
	if got := ConversationKeyOf(req); got != "" {
		t.Fatalf("ConversationKeyOf = %q, want empty", got)
	}
	if got := DeriveConversationKey(req.Messages); got == "" {
		t.Fatal("the content fallback is the whole point and it derived nothing")
	}
}

// TestDeriveConversationKeyIsNamespaced: a derived key must be recognisable as
// derived, so it can never be confused with an id a client really sent.
func TestDeriveConversationKeyIsNamespaced(t *testing.T) {
	got := DeriveConversationKey([]Message{{Role: "user", Content: "hello"}})
	if !strings.HasPrefix(got, DerivedKeyPrefix) {
		t.Fatalf("key = %q, want the %q prefix", got, DerivedKeyPrefix)
	}
	// 16 bytes of SHA-256 as hex.
	if body := strings.TrimPrefix(got, DerivedKeyPrefix); len(body) != 32 {
		t.Fatalf("key body = %q (%d chars), want 32 hex chars", body, len(body))
	}
}

// TestDeriveConversationKeyIsStableAcrossTurns is the property the whole feature
// rests on: a multi-turn conversation appends history every turn, and if the key
// moved with it stickiness would be worse than useless.
func TestDeriveConversationKeyIsStableAcrossTurns(t *testing.T) {
	base := []Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
	}
	first := DeriveConversationKey(base)

	grown := append([]Message{}, base...)
	grown = append(grown,
		Message{Role: "assistant", Content: "hi there"},
		Message{Role: "user", Content: "what is 2+2"},
		Message{Role: "assistant", Content: "4"},
		Message{Role: "user", Content: "and 3+3"},
	)
	if got := DeriveConversationKey(grown); got != first {
		t.Fatalf("the key drifted across turns: %q then %q", first, got)
	}

	// A later system message must not displace the first one either: the first
	// is what the conversation opened with.
	respelled := append([]Message{{Role: "system", Content: "you are helpful"}},
		Message{Role: "system", Content: "actually be terse"},
		Message{Role: "user", Content: "hello"},
	)
	if got := DeriveConversationKey(respelled); got != first {
		t.Fatalf("a later system turn changed the key: %q vs %q", got, first)
	}
}

// TestDeriveConversationKeySeparatesConversations: two conversations sharing a
// system prompt must not collapse onto one account.
func TestDeriveConversationKeySeparatesConversations(t *testing.T) {
	one := DeriveConversationKey([]Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
	})
	two := DeriveConversationKey([]Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "goodbye"},
	})
	if one == two {
		t.Fatalf("two conversations derived the same key %q", one)
	}
	// The system prompt is half the signature, so changing it alone is a
	// different conversation too.
	three := DeriveConversationKey([]Message{
		{Role: "system", Content: "you are terse"},
		{Role: "user", Content: "hello"},
	})
	if three == one {
		t.Fatalf("a different system prompt derived the same key %q", one)
	}
	// "developer" is the same slot as "system".
	four := DeriveConversationKey([]Message{
		{Role: "developer", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
	})
	if four != one {
		t.Fatalf("developer = %q, system = %q, want the same", four, one)
	}
}

// TestDeriveConversationKeyIgnoresImageURLChurn: the same logical image arrives
// with a different signed URL on every turn, so the URL must not reach the key.
// The placeholder still keeps "no image" apart from "one image".
func TestDeriveConversationKeyIgnoresImageURLChurn(t *testing.T) {
	withImage := func(url string) []Message {
		return []Message{{Role: "user", Parts: []ContentPart{
			{Type: "text", Text: "what is this"},
			{Type: "image_url", ImageURL: url, Detail: "high"},
		}}}
	}
	first := DeriveConversationKey(withImage("https://cdn/x.png?sig=aaa"))
	second := DeriveConversationKey(withImage("https://cdn/x.png?sig=bbb"))
	if first != second {
		t.Fatalf("the key drifted with the image URL: %q then %q", first, second)
	}
	textOnly := DeriveConversationKey([]Message{{Role: "user", Parts: []ContentPart{
		{Type: "text", Text: "what is this"},
	}}})
	if textOnly == first {
		t.Fatal("a text-only turn derived the same key as an image turn")
	}
}

// TestDeriveConversationKeyHandlesAnImageOnlyFirstTurn: a first user turn with
// no text part at all must still derive, or the very clients this fallback
// exists for would be the ones it silently skips.
func TestDeriveConversationKeyHandlesAnImageOnlyFirstTurn(t *testing.T) {
	got := DeriveConversationKey([]Message{{Role: "user", Parts: []ContentPart{
		{Type: "image_url", ImageURL: "https://cdn/x.png"},
	}}})
	if !strings.HasPrefix(got, DerivedKeyPrefix) {
		t.Fatalf("an image-only first turn derived %q", got)
	}
	two := DeriveConversationKey([]Message{{Role: "user", Parts: []ContentPart{
		{Type: "image_url", ImageURL: "https://cdn/a.png"},
		{Type: "image_url", ImageURL: "https://cdn/b.png"},
	}}})
	if two == got {
		t.Fatal("one image and two images derived the same key")
	}
}

// TestDeriveConversationKeyNeedsAUserTurn: with nothing to attribute the
// conversation to there is no key, which means "no stickiness, plain rotation" —
// a safe degradation rather than a fabricated key.
func TestDeriveConversationKeyNeedsAUserTurn(t *testing.T) {
	cases := []struct {
		name string
		msgs []Message
	}{
		{"no messages", nil},
		{"system only", []Message{{Role: "system", Content: "you are helpful"}}},
		{"assistant only", []Message{{Role: "assistant", Content: "hi"}}},
		{"empty user text", []Message{{Role: "user", Content: "   "}}},
		{"user with no parts and no text", []Message{{Role: "user"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveConversationKey(tc.msgs); got != "" {
				t.Fatalf("key = %q, want empty", got)
			}
		})
	}
}

// TestDeriveConversationKeyWorksWithoutASystemPrompt: half a signature is still
// a signature.  The first user turn alone distinguishes the conversation.
func TestDeriveConversationKeyWorksWithoutASystemPrompt(t *testing.T) {
	got := DeriveConversationKey([]Message{{Role: "user", Content: "hello"}})
	if !strings.HasPrefix(got, DerivedKeyPrefix) {
		t.Fatalf("key = %q, want a derived key", got)
	}
	if other := DeriveConversationKey([]Message{{Role: "user", Content: "hello "}}); other != got {
		t.Fatalf("trailing space changed the key: %q vs %q", other, got)
	}
}
