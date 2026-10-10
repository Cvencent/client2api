package core

// affinity.go — conversation → account stickiness, offered as an opt-in
// capability rather than as a global mechanism.
//
// Why this exists
//
// Several vendors salt their prompt cache per account.  clients/workbuddy feeds
// the account uid into the vendor's prompt_cache_key (see InjectPromptCacheKey),
// so the same conversation served by two different accounts produces two
// different cache keys and the vendor re-bills the whole prefix.  Rotating
// accounts per request is therefore not merely suboptimal for those modules, it
// is a cost regression.  Conversation→account affinity pins a conversation to
// the account that has already warmed its cache for it.
//
// Why it is additive and nil-safe
//
// Nothing here changes an existing type, signature or behaviour.  A module that
// never mentions this file keeps working exactly as before, and a module that
// adopts it stores an *Affinity in a field that may legitimately stay nil:
// every method begins with a nil receiver check and answers "no binding" or
// does nothing.  That is what lets a client opt in with one field and no
// constructor change, and lets a client that never opts in compile unchanged.
//
// The table holds no pool knowledge on purpose.  Liveness is supplied by the
// caller through the usable predicate of Resolve, so core never has to learn
// what "healthy" means for any particular vendor, and a binding to a parked
// account is dropped instead of being served.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAffinityTTL is how long an unused conversation binding survives.
	// It matches the reference implementation's session TTL (30 minutes): long
	// enough to cover a conversation's pauses, short enough that a table built
	// from a day of traffic still fits in memory.
	DefaultAffinityTTL = 30 * time.Minute
	// DefaultAffinityGCInterval is how often StartGC sweeps expired bindings.
	// The lazy sweep in Resolve already covers actively used keys; this is what
	// keeps the table from growing with conversations that went quiet.
	DefaultAffinityGCInterval = 5 * time.Minute
)

// affinityEntry is one binding plus the moment it was last used.  The timestamp
// is refreshed on every successful Resolve, so the TTL is an *idle* timeout,
// not an absolute lifetime.
type affinityEntry struct {
	account    string
	lastActive time.Time
}

// Affinity is a concurrency-safe conversation→account binding table.
//
// The zero value is not usable; call NewAffinity.  A nil *Affinity is usable
// and behaves as an empty table, which is the supported way for a module to say
// "I do not do stickiness".
type Affinity struct {
	mu      sync.RWMutex
	entries map[string]affinityEntry
	ttl     time.Duration
	gcEvery time.Duration
	now     func() time.Time

	// disabled is the inverse of the operator's session_sticky.enabled.  It is
	// stored inverted on purpose: stickiness is on by default (the reference
	// sets session_sticky.enabled to true when the file is silent), and an
	// inverted flag makes the zero value mean "on" as well, so an Affinity
	// reached through a struct literal -- not only through NewAffinity -- still
	// starts in the documented state rather than silently losing stickiness.
	disabled bool

	// gcMu guards only the GC goroutine's lifecycle, so StartGC/StopGC never
	// contend with the request path.
	gcMu   sync.Mutex
	stopGC chan struct{}
}

// NewAffinity returns an empty table.  A non-positive ttl selects
// DefaultAffinityTTL.
func NewAffinity(ttl time.Duration) *Affinity {
	if ttl <= 0 {
		ttl = DefaultAffinityTTL
	}
	return &Affinity{
		entries: make(map[string]affinityEntry),
		ttl:     ttl,
		gcEvery: DefaultAffinityGCInterval,
		now:     time.Now,
	}
}

// TTL reports the idle timeout in force.  It is nil-safe.
func (a *Affinity) TTL() time.Duration {
	if a == nil {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ttl
}

// GCInterval reports how often the background sweep runs.  It is nil-safe.
func (a *Affinity) GCInterval() time.Duration {
	if a == nil {
		return 0
	}
	a.gcMu.Lock()
	defer a.gcMu.Unlock()
	return a.gcEvery
}

// SetTTL changes the idle timeout.  A non-positive duration selects
// DefaultAffinityTTL, exactly as NewAffinity does, so an operator who clears the
// value gets the documented default rather than a window of zero (which would
// mean "no stickiness at all" by accident).  It is nil-safe.
func (a *Affinity) SetTTL(d time.Duration) {
	if a == nil {
		return
	}
	if d <= 0 {
		d = DefaultAffinityTTL
	}
	a.mu.Lock()
	a.ttl = d
	a.mu.Unlock()
}

// SetGCInterval changes how often the background sweep runs, restarting a
// running sweep so the new cadence takes effect immediately.  A non-positive
// duration selects DefaultAffinityGCInterval, the same rule as NewAffinity.  It
// is nil-safe.
func (a *Affinity) SetGCInterval(d time.Duration) {
	if a == nil {
		return
	}
	if d <= 0 {
		d = DefaultAffinityGCInterval
	}
	// gcEvery is guarded by gcMu because that is the lock StartGC reads it
	// under: the ticker bakes the interval in at launch, so the two must not
	// disagree.
	a.gcMu.Lock()
	a.gcEvery = d
	running := a.stopGC != nil
	a.gcMu.Unlock()
	if !running {
		return
	}
	a.StopGC()
	a.StartGC()
}

// Enabled reports whether stickiness is currently on.  A nil *Affinity reports
// false: a module that never allocated a table is a module that does not do
// stickiness, so there is nothing to be enabled.
func (a *Affinity) Enabled() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return !a.disabled
}

// SetEnabled turns stickiness on or off, mirroring the reference's
// session_sticky.enabled, which does not shorten the window but removes the
// routing step entirely: with the reference's value off there is no session
// router at all and every request selects an account from scratch.
//
// Disabling therefore also drops every binding.  A disabled table must not
// accumulate entries that would resurface the moment an operator turns the
// switch back on, because those bindings were made against a routing policy
// that is no longer in force -- the reference, having never built the router,
// has no such memory to resurrect either.
//
// It is nil-safe.
func (a *Affinity) SetEnabled(enabled bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.disabled = !enabled
	if !enabled {
		a.entries = nil
	}
}

// Bind pins conversationKey to accountID, replacing any previous binding.  It is
// a no-op when either side is empty, and it is nil-safe.
func (a *Affinity) Bind(conversationKey, accountID string) {
	if a == nil || conversationKey == "" || accountID == "" {
		return
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.disabled {
		return
	}
	if a.entries == nil {
		a.entries = make(map[string]affinityEntry)
	}
	a.entries[conversationKey] = affinityEntry{account: accountID, lastActive: now}
}

// Resolve returns the account bound to conversationKey, and reports whether a
// usable binding existed.
//
// usable answers "may this account serve the request right now?" and may be
// nil, in which case a binding is trusted for as long as it has not expired.
// A binding that is unknown, expired, or pointed at an account the caller
// reports as unusable is DELETED and reported as absent, so the caller falls
// through to its normal selection and re-binds on the next success.  That is
// the whole fallback rule: stickiness never serves a parked account and never
// fails a request.
//
// Resolve also refreshes the binding's last-active time, which makes the TTL an
// idle timeout.  It is nil-safe.
func (a *Affinity) Resolve(conversationKey string, usable func(accountID string) bool) (string, bool) {
	if a == nil || conversationKey == "" {
		return "", false
	}
	now := a.now()

	a.mu.RLock()
	e, ok := a.entries[conversationKey]
	off := a.disabled
	// Sample the TTL while the lock is held; see expired.
	ttl := a.ttl
	a.mu.RUnlock()
	if off || !ok {
		return "", false
	}
	if !expired(e, now, ttl) && (usable == nil || usable(e.account)) {
		a.mu.Lock()
		// Re-read: another goroutine may have re-bound the key in between, in
		// which case its fresher entry wins and must not be touched.
		if cur, still := a.entries[conversationKey]; still && cur.account == e.account {
			cur.lastActive = now
			a.entries[conversationKey] = cur
		}
		a.mu.Unlock()
		return e.account, true
	}

	a.mu.Lock()
	if cur, still := a.entries[conversationKey]; still && cur.account == e.account {
		delete(a.entries, conversationKey)
	}
	a.mu.Unlock()
	return "", false
}

// Unbind forgets one conversation, reporting whether a binding was there.  It is
// nil-safe.
func (a *Affinity) Unbind(conversationKey string) bool {
	if a == nil || conversationKey == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.entries[conversationKey]; !ok {
		return false
	}
	delete(a.entries, conversationKey)
	return true
}

// Count reports how many bindings are held, expired ones included.  It is
// nil-safe.
func (a *Affinity) Count() int {
	if a == nil {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.entries)
}

// GC drops every expired binding and reports how many it removed.  It is
// nil-safe, and it is safe to call concurrently with Resolve.
func (a *Affinity) GC() int {
	if a == nil {
		return 0
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	removed := 0
	for k, e := range a.entries {
		if expired(e, now, a.ttl) {
			delete(a.entries, k)
			removed++
		}
	}
	return removed
}

// expired reports whether an entry has been idle for longer than the TTL.
//
// ttl is passed in rather than read from a.ttl here because the callers hold
// different locks: GC runs under a.mu.Lock, but Resolve is on the hot request
// path and deliberately drops the read lock before it evaluates the entry, so
// that the refresh below cannot be starved behind a long usable() callback.
// Reading a.ttl without a lock on that path is a data race against SetTTL, which
// an operator can fire from the panel at any moment.  The caller therefore
// samples ttl while it still holds the lock and hands the value in; a stale
// sample only costs one interval of stickiness, never correctness.
func expired(e affinityEntry, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	return now.Sub(e.lastActive) > ttl
}

// StartGC launches the background sweep.  It is idempotent, and it is nil-safe.
//
// The ticker goroutine captures its own stop channel into a local before it
// starts: reading a.stopGC from inside the goroutine would race with StopGC,
// and closing a channel the goroutine had already read as nil would block
// forever on a receive that can never be satisfied.
func (a *Affinity) StartGC() {
	if a == nil {
		return
	}
	a.gcMu.Lock()
	defer a.gcMu.Unlock()
	if a.stopGC != nil {
		return
	}
	stop := make(chan struct{})
	a.stopGC = stop
	every := a.gcEvery
	if every <= 0 {
		every = DefaultAffinityGCInterval
	}
	// GoSafe, not a bare "go": this sweep runs for the life of the process, so
	// a panic in it would take down every request in flight.  There is no
	// logger here to hand the trace to, so it goes to stderr.
	GoSafe("affinity sweep", nil, func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				a.GC()
			}
		}
	})
}

// StopGC stops the background sweep.  Calling it without StartGC, or twice, is
// harmless.  It is nil-safe.
func (a *Affinity) StopGC() {
	if a == nil {
		return
	}
	a.gcMu.Lock()
	defer a.gcMu.Unlock()
	if a.stopGC == nil {
		return
	}
	close(a.stopGC)
	a.stopGC = nil
}

// ConversationBinder is the optional capability a module implements when its
// callers may need to inspect or clear conversation stickiness from outside --
// an administrative "forget this session" hook, or status reporting.
//
// It is discovered by type assertion, exactly like HealthProvider and
// Degrader, so implementing it is optional and omitting it changes nothing.
type ConversationBinder interface {
	// BindConversation pins conversationKey to accountID.
	BindConversation(conversationKey, accountID string)
	// UnbindConversation forgets conversationKey, reporting whether it was bound.
	UnbindConversation(conversationKey string) bool
	// ConversationAccount reports the account currently bound to a key, and only
	// when that account is still able to serve model right now.  An unusable
	// binding must be reported as absent -- the caller's contract is the same as
	// Affinity.Resolve's: no answer means "pick normally", never "fail".
	//
	// model is the model the caller is about to request and may be empty when the
	// caller does not know it; a module whose health is model-scoped should then
	// fall back to its model-agnostic answer rather than refuse to answer.
	ConversationAccount(conversationKey, model string) (string, bool)
}

// AsConversationBinder reports whether a client offers stickiness management.
// A client that does not implement ConversationBinder keeps working unchanged;
// callers must treat ok == false as "no such capability", never as an error.
func AsConversationBinder(c Client) (ConversationBinder, bool) {
	if c == nil {
		return nil, false
	}
	b, ok := c.(ConversationBinder)
	return b, ok
}

// ConversationKey derives a sticky conversation key from a request's options,
// using the same key spellings the modules and the reference accept.  An empty
// result means "not a conversation-scoped request": it is not a failure, and
// the caller should simply skip stickiness for that request.
//
// user is the optional final fallback and is supplied by the caller rather than
// read from a request, because whether a user identifier is a useful stickiness
// key is a per-module decision.
func ConversationKey(options map[string]any, user string) string {
	for _, key := range []string{"conversation_id", "conversationId", "prompt_cache_key"} {
		if s := optionText(options, key); s != "" {
			return s
		}
	}
	return strings.TrimSpace(user)
}

// ConversationKeyOf resolves the stickiness key for a whole request: the
// conversation id the gateway already resolved (from the metadata object or the
// top level) when the caller supplied one, the raw option spellings (or the
// request user), and finally a stable key derived from the system prompt plus
// the first user turn.
//
// Modules should prefer this over ConversationKey.  A client that follows the
// OpenAI convention puts its conversation id in metadata, where the option
// spellings cannot see it; keying off options alone would silently drop
// stickiness for exactly the clients that are most explicit about their
// conversations.
//
// The content fallback is what makes stickiness useful for clients that never
// send a conversation id.  It returns "" only when there is genuinely nothing
// stable to hash, which is the safe degradation: no key means no binding and
// ordinary rotation.
func ConversationKeyOf(req *ChatRequest) string {
	if req == nil {
		return ""
	}
	if id := strings.TrimSpace(req.ConversationID); id != "" {
		return id
	}
	if k := ConversationKey(req.Options, req.User); k != "" {
		return k
	}
	return DeriveConversationKey(req.Messages)
}

// DerivedKeyPrefix namespaces a content-derived key so it can never be confused
// with a conversation id a client actually sent.  The explicit id is checked
// first, so a client that genuinely sends "d-<hex>" still wins and the two
// namespaces never compete.
const DerivedKeyPrefix = "d-"

// DeriveConversationKey derives a stable stickiness key from message content, for
// the clients that send no conversation id at all.  It returns "" when there is
// nothing to derive from, which means "no key, no stickiness, plain rotation" —
// a safe degradation, never a failure.
//
// Why the system prompt plus the first user turn and nothing else:
//
//   - a multi-turn conversation appends history on every turn, so hashing the
//     whole message list would mint a new key per turn and stick nothing at all;
//   - the system prompt and the first user turn stay constant for the life of a
//     conversation, which is the same scope upstream prompt caching uses.
//
// Both halves may legitimately be empty: a conversation with no system prompt
// still derives, and a first turn that is nothing but an image still derives
// (see contentSignature).  Only a first user turn with no content whatsoever
// yields no key.
func DeriveConversationKey(msgs []Message) string {
	systemText, firstUser := "", ""
	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			if systemText == "" {
				systemText = strings.TrimSpace(m.Content)
			}
		case "user":
			if firstUser == "" {
				firstUser = contentSignature(m)
			}
		}
		if systemText != "" && firstUser != "" {
			break
		}
	}
	if firstUser == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUser))
	return DerivedKeyPrefix + hex.EncodeToString(sum[:16])
}

// contentSignature renders one message as the session-stable half of a derived
// key.
//
// Text parts are concatenated as they are.  A non-text part contributes only a
// "[type]" placeholder and never its content, because the same logical image
// arrives with a different signed URL — or a re-encoded base64 body — on every
// turn, and letting that into the key would make it drift per turn and stick
// nothing.  The placeholder still tells "no image" from "one image" from "two",
// so a first turn that is nothing but an image derives a key instead of falling
// back to plain rotation.
//
// A message parsed from a plain string carries no parts at all, so its text is
// the whole signature.
func contentSignature(m Message) string {
	if len(m.Parts) == 0 {
		return strings.TrimSpace(m.Content)
	}
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == "" || p.Type == "text" {
			b.WriteString(p.Text)
			continue
		}
		b.WriteString("[" + p.Type + "]")
	}
	return strings.TrimSpace(b.String())
}

// optionText renders one option as a non-empty trimmed string.  Numeric JSON
// identifiers are accepted (a conversation id is as often 123456 as "abc"), but
// structured values are deliberately ignored: they cannot be a key.
func optionText(options map[string]any, key string) string {
	v, ok := options[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return ""
	}
}
