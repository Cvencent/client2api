// Package core defines the frozen contract that every client module implements.
//
// DEPENDENCY RULE (this is what makes the modules independent):
//
//	nothing under internal/ or cmd/ may import a clients/* package, and no
//	clients/* package may import another clients/* package.  Every arrow
//	points inward, to this package.  A client module may therefore be
//	rewritten, replaced or deleted without any other module noticing.
//
// A client module owns everything specific to its vendor: credential
// acquisition and refresh, account pool and selection policy, upstream
// protocol, request/response translation, retries and cooldowns.  The core
// owns only the vendor-neutral surface: the OpenAI-compatible HTTP API, model
// routing, the panel, and the Deps plumbing.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ErrNotConfigured is returned by a module that is compiled in but has no
// usable credential or configuration yet.
var ErrNotConfigured = errors.New("client is not configured (no account)")

// ErrUnsupported is returned for a request shape the module cannot express
// upstream.
var ErrUnsupported = errors.New("unsupported by this client")

// ErrBusy reports that the client is healthy but has no free in-flight slot
// right now.
//
// It is deliberately not ErrNotConfigured: an account exists and works, so the
// right answer is "back off and retry" (the gateway maps this to 429) rather
// than "fix your configuration".  A module returns it when every usable account
// is at its concurrency ceiling, which is the state the reference reports as
// in_flight_full.
var ErrBusy = errors.New("client is at its in-flight limit")

// ErrPlatformExhausted reports that a module has given up on its platform for
// this request.  The gateway must then move to the next platform instead of
// re-running the same platform's account rotation.  Modules that do not return
// it keep the previous retry behaviour.
//
// The headline deliberately makes no claim about how many accounts were asked.
// Most modules stop the walk early when the refusal is not about the account --
// an oversized body, an unsupported model, a malformed tool schema -- because a
// sibling account would be refused identically.  The text used to read "every
// usable account on this platform failed", which sent the operator looking for
// a health problem after a single account had been tried; the accurate count is
// the module's to add, because only the module knows how far it walked.
var ErrPlatformExhausted = errors.New("platform exhausted")

// ---------------------------------------------------------------------------
// Canonical request types.  These are deliberately vendor-neutral: the gateway
// translates the OpenAI wire format into them, and each module translates them
// into whatever its upstream wants.
// ---------------------------------------------------------------------------

// ContentPart is one element of a multimodal message.
type ContentPart struct {
	Type     string // "text" | "image_url"
	Text     string
	ImageURL string
	Detail   string
}

// Message is one conversation turn.  Content holds the flattened text; Parts
// holds any non-text parts, in order, when the caller sent an array.
type Message struct {
	Role       string
	Content    string
	Parts      []ContentPart
	ToolCalls  []ToolCall
	ToolCallID string
	Name       string
	Reasoning  string
}

// ToolCall is an assistant-issued tool invocation.
type ToolCall struct {
	ID        string
	Type      string
	Name      string
	Arguments string
}

// Tool is a tool declaration offered to the model.
type Tool struct {
	Type        string
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ChatRequest is a normalised completion request.
type ChatRequest struct {
	Model       string
	Messages    []Message
	Tools       []Tool
	ToolChoice  json.RawMessage
	Temperature *float64
	TopP        *float64
	MaxTokens   *int
	Stop        []string
	Stream      bool
	User        string
	// ConversationID is the caller-supplied conversation identifier.  The
	// gateway resolves it from the request's metadata object or its top level,
	// so a module never has to guess which spelling arrived.
	//
	// It is deliberately NOT folded into Options: modules copy Options verbatim
	// into the body they send upstream, and a key only this side understands
	// must never reach the vendor.
	ConversationID string
	// ConversationRequestID is the caller's own conversation-turn id, passed
	// through when the request carried one.  A module that aggregates its
	// upstream calls per turn derives a value itself when this is empty.
	ConversationRequestID string
	// FreeModel marks a request the router resolved to a model its platform
	// advertises as free (a zero multiplier).  A pool that parks an account for
	// credit exhaustion must treat such a request as still routable: a model the
	// vendor charges nothing for does not need the balance, and the only
	// evidence that disqualifies an account is the vendor refusing *this* model
	// on *this* account.  Parks that are not about money -- a WAF block, a dead
	// session, a risk flag, a rate limit -- keep applying.
	//
	// Like ConversationID and ServedBy this is a gateway-to-module channel only.
	// It must never be copied into the body sent upstream.
	FreeModel bool
	// Options carries per-client knobs.  Callers set it via the
	// "client2api" object in the JSON body.
	Options map[string]any
	// ServedBy, when non-nil, is where a module writes the id of the account
	// that actually served this request.  The gateway allocates it per request
	// and reads it once Chat has returned, so a module that rotates through
	// credentials overwrites it on every attempt and the last writer wins.
	//
	// It exists because the gateway deliberately does not guess: it cannot tell
	// which credential a module picked, and both the usage ledger's per-account
	// grouping and the per-request console row need to know.  A module that
	// leaves it untouched reports "unknown", which is honest; it must never
	// write an account it did not use.
	//
	// Like ConversationID this is a gateway-to-module channel only.  It must
	// never be copied into the body sent upstream.
	ServedBy *string

	// EffortFinal marks a reasoning level the gateway deliberately chose for
	// this candidate. A module must not re-snap it against its own cached
	// ladder: the panel's manual vocabulary is authoritative and may be newer
	// than that cache. Like ServedBy, this never goes upstream.
	EffortFinal bool

	// ContextWindowLimit, when positive, caps the context window the request
	// guard will trim against.  It exists so the panel's chat tester can ask
	// "does this model actually accept N tokens?" without persisting a manual
	// override: the effective window is the smaller of the configured/vendor
	// window and this test limit.  It is a gateway-only value and must never
	// reach a module's upstream body.
	ContextWindowLimit int64

	// accountAcquire is the gateway's per-account in-flight hook.  The
	// gateway installs it before every attempt; a module calls
	// AcquireAccountSlot right after it picks a credential and before any
	// upstream call.  It is unexported because only the methods below may
	// touch it, and nil when no gateway is installed (unit tests, embedders).
	accountAcquire func(accountID string) (func(), error)
	// accountRelease is the lease AcquireAccountSlot is currently holding, or
	// nil.  The gateway returns it when the module gives the slot back or when
	// the stream it produced is closed.
	accountRelease func()
	// accountFailed is the gateway's per-attempt failure hook.  A module that
	// rotates through several accounts calls NoteAccountFailure after every
	// upstream failure, so the gateway can park that (account, model) pair even
	// when the module recovers on the next account.  Unexported for the same
	// reason as accountAcquire.
	accountFailed func(accountID string, err error)
}

// NoteServedBy records the credential that served a request in the slot the
// gateway allocated on ChatRequest.ServedBy.  A nil request, a request whose
// slot the caller never allocated, and an empty account id are all no-ops, so
// a module can call this unconditionally at the point it picks an account and
// "the module did not say" stays distinguishable from "the module said blank".
func NoteServedBy(req *ChatRequest, accountID string) {
	if req == nil || req.ServedBy == nil || accountID == "" {
		return
	}
	*req.ServedBy = accountID
}

// SetAccountAcquirer installs the per-account in-flight hook.  It is called by
// the gateway once per attempt; a module never calls it.  A nil fn disables the
// gate, which is the state a unit test or an embedder runs in.
func (r *ChatRequest) SetAccountAcquirer(fn func(accountID string) (func(), error)) {
	if r == nil {
		return
	}
	r.accountAcquire = fn
}

// SetAccountFailureReporter installs the per-attempt failure hook.  It is
// called by the gateway once per attempt; a module never calls it.  A nil fn
// disables the hook, which is the state a unit test or an embedder runs in.
func (r *ChatRequest) SetAccountFailureReporter(fn func(accountID string, err error)) {
	if r == nil {
		return
	}
	r.accountFailed = fn
}

// NoteAccountFailure reports one upstream failure the module absorbed, so the
// gateway can park that (account, model) pair even when a later account served
// the request.  A module calls it right after it decides an attempt failed and
// before it rotates.  A nil request, a missing hook, an empty account id and a
// nil error are no-ops, so a module can call it unconditionally.
func (r *ChatRequest) NoteAccountFailure(accountID string, err error) {
	if r == nil || r.accountFailed == nil || accountID == "" || err == nil {
		return
	}
	r.accountFailed(accountID, err)
}

// AcquireAccountSlot takes the per-account in-flight slot for accountID.  A
// module calls it immediately after picking a credential and before the
// upstream call, so the ceiling is enforced before the vendor ever sees the
// request.  It returns core.ErrBusy when the account is at its configured
// ceiling: the module should pick another account and try again.
//
// Moving to another account is implicit: once the new slot is taken, the
// previous lease is returned, so a rotating module only ever holds one slot.
// A refused acquisition leaves the previous lease in place.
//
// It is a no-op when no hook is installed or accountID is empty, so a module
// can call it unconditionally.
func (r *ChatRequest) AcquireAccountSlot(accountID string) error {
	if r == nil || r.accountAcquire == nil || accountID == "" {
		return nil
	}
	release, err := r.accountAcquire(accountID)
	if err != nil {
		return err
	}
	if r.accountRelease != nil {
		r.accountRelease()
	}
	r.accountRelease = release
	return nil
}

// ReleaseAccountSlot returns the lease AcquireAccountSlot holds.  The gateway
// calls it when Chat refused, and the wrapped stream calls it on Close; a
// module may also call it when it abandons an attempt.  It is idempotent.
func (r *ChatRequest) ReleaseAccountSlot() {
	if r == nil || r.accountRelease == nil {
		return
	}
	release := r.accountRelease
	r.accountRelease = nil
	release()
}

// ClearAccountHook drops the per-account hook and lease without calling the
// release.  The gateway uses it on a failover clone, which must not inherit
// the original request's hook or share its lease; the original keeps its own
// lease and returns it through ReleaseAccountSlot.
func (r *ChatRequest) ClearAccountHook() {
	if r == nil {
		return
	}
	r.accountAcquire = nil
	r.accountRelease = nil
	r.accountFailed = nil
}

// Usage is token accounting reported by the upstream.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
	// CachedTokensKnown distinguishes a reported zero from a protocol that
	// omitted cache accounting entirely. The value alone cannot: both decode
	// to zero, but only the first is a real cache miss.
	CachedTokensKnown bool `json:"-"`
}

// Model is one model a module can serve.
type Model struct {
	ID      string
	OwnedBy string
	Extra   map[string]any
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// EventType discriminates Event.
type EventType string

const (
	EventDelta    EventType = "delta"
	EventToolCall EventType = "tool_call"
	EventUsage    EventType = "usage"
	EventDone     EventType = "done"
	EventError    EventType = "error"
)

// ToolCallDelta is one incremental tool-call fragment.  Index groups fragments
// belonging to the same call.
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// Event is one item on a completion stream.
type Event struct {
	Type      EventType
	Delta     string
	Reasoning string
	ToolCall  *ToolCallDelta
	Usage     *Usage
	Finish    string
	Err       error
}

// Stream is one chat completion attempt.  Recv returns io.EOF exactly once at
// the clean end of a stream; any other error is terminal and carries the
// upstream's own message.  Close must be safe to call more than once.
type Stream interface {
	Recv() (Event, error)
	Close() error
}

// ---------------------------------------------------------------------------
// Status / introspection, rendered by the panel
// ---------------------------------------------------------------------------

// AccountStatus is one credential's health, for the panel.
type AccountStatus struct {
	ID        string         `json:"id"`
	Label     string         `json:"label,omitempty"`
	Enabled   bool           `json:"enabled"`
	State     string         `json:"state"` // ready|cooling|exhausted|invalid|unknown
	ExpiresAt string         `json:"expires_at,omitempty"`
	Note      string         `json:"note,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`
	// Identity is the vendor's own id for the account this credential belongs
	// to, when the module can determine it.  It carries the same meaning as
	// AccountRecord.Identity, and is what lets a panel show several credentials
	// for one account as channels of it rather than as separate accounts.
	Identity string `json:"identity,omitempty"`
}

// ModelPark is one model a pool has parked on one account right now.  A pool
// with a per-model cooldown publishes these in AccountStatus.Extra under the
// "model_cooldowns" key so the panel can answer "which models can this
// account
// serve at this instant" without parsing the module's own prose.
//
// It is structured rather than a rendered sentence on purpose: the panel
// labels a vendor rate-limit window and a deterministic "this model is not
// available here" refusal differently, and it has to time the first one.  The
// key must be present -- even as an empty slice -- on every account of a
// module that parks models: its absence is how the panel tells "nothing is
// parked" apart from "this module does not report model-level limits".
type ModelPark struct {
	Model string `json:"model"`
	// Kind is ModelParkRateLimit for a timed window or ModelParkUnsupported
	// for a model the vendor refuses on this account.
	Kind   string `json:"kind"`
	Reason string `json:"reason,omitempty"`
	// Until is when the park lapses, RFC3339 UTC.  Empty means the module
	// could not say.
	Until string `json:"until,omitempty"`
	// ResetAt is the vendor's own reset instant when it gave one.  It is kept
	// apart from Until because "the vendor said 14:30" and "our backoff runs
	// out at 14:30" read differently to an operator.
	ResetAt string `json:"reset_at,omitempty"`
}

// The two kinds a ModelPark can be, shared by the pools that raise parks and
// the panel that renders them.
const (
	ModelParkRateLimit   = "rate_limit"
	ModelParkUnsupported = "unsupported"
)

// NewModelPark builds one wire entry from a module's own park record.  until
// and resetAt may be zero; a zero time is omitted rather than serialised as
// "0001-01-01".
func NewModelPark(model, kind, reason string, until, resetAt time.Time) ModelPark {
	p := ModelPark{Model: model, Kind: kind, Reason: reason}
	if !until.IsZero() {
		p.Until = until.UTC().Format(time.RFC3339)
	}
	if !resetAt.IsZero() {
		p.ResetAt = resetAt.UTC().Format(time.RFC3339)
	}
	return p
}

// SortModelParks puts a park list in a stable order and guarantees it is
// non-nil, so a module with nothing parked publishes [] rather than null.
// The panel renders an empty list and a missing list differently.
func SortModelParks(parks []ModelPark) []ModelPark {
	out := make([]ModelPark, 0, len(parks))
	out = append(out, parks...)
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// Status is a module's self-report.
type Status struct {
	Name      string          `json:"name"`
	Label     string          `json:"label,omitempty"`
	Ready     bool            `json:"ready"`
	Detail    string          `json:"detail,omitempty"`
	Accounts  []AccountStatus `json:"accounts,omitempty"`
	Models    []string        `json:"models,omitempty"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// The contract
// ---------------------------------------------------------------------------

// Client is the single interface a client module must satisfy.  Name is the
// routing prefix: a request for model "zcode/GLM-5.3" goes to the module
// registered as "zcode", which receives Model == "GLM-5.3".
type Client interface {
	Name() string
	Models(ctx context.Context) ([]Model, error)
	Chat(ctx context.Context, req *ChatRequest) (Stream, error)
	Status(ctx context.Context) Status
}

// Deps is everything the core hands a module at construction time.  It is
// intentionally tiny so that adding a core feature never breaks a module.
type Deps struct {
	// DataDir is the module's private directory, already created.  Modules
	// must keep all of their state inside it and nowhere else.
	DataDir string
	// Config is the raw JSON for this module out of the "clients" object of
	// the main config file.  Nil when absent.  Each module owns its schema.
	Config json.RawMessage
	// HTTPClient is a shared client honouring the configured proxy and with
	// sane timeouts.  Modules that need a different policy may build their
	// own.
	HTTPClient *http.Client
	// Proxy is the proxy URL the gateway was configured with, empty for a
	// direct connection.  A module that builds its own client needs it to keep
	// honouring the proxy; a module that keeps HTTPClient can ignore it.
	Proxy string
	// Guard is the process-level state that no single account owns: the
	// egress-IP WAF gate and the degraded-prompt window.  Every module shares
	// one instance, so a WAF block seen by one client stops the others too.
	//
	// It is nil when the process did not build one (unit tests), so a module
	// must nil-check.  Guard's methods are themselves nil-safe.
	Guard *Guard
	// Logf is a scoped logger already prefixed with the module name.
	Logf func(format string, args ...any)
}

// Log is a nil-safe convenience wrapper.
func (d Deps) Log(format string, args ...any) {
	if d.Logf != nil {
		d.Logf(format, args...)
	}
}

// Factory builds a module instance.
type Factory func(deps Deps) (Client, error)

// RepairToolArguments makes every assistant tool call's arguments acceptable to
// a vendor that parses them as JSON.
//
// Chat encodes a call's arguments as a string that holds JSON, and strict
// relays reject the whole request when it does not parse:
//
//	Assistant tool call function.arguments must be valid JSON.
//
// That string is not always clean.  Codex replays its own transcript, and
// transcripts observed in practice contain a missing quote (`"prefix_rule":
// pwsh`), a leaked control token, a truncated diff, and calls whose payload was
// freeform text to begin with.  Repairing here -- the one place the gateway
// normalises history before every candidate -- means one damaged turn no longer
// costs every failover sibling in the priority order.
//
// The repair is conservative and always additive: a value that already parses as
// a JSON object or array is returned byte for byte, and anything else is
// preserved under "_raw" so the tool result that follows still has its call.
func RepairToolArguments(msgs []Message) []Message {
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			msgs[i].ToolCalls[j].Arguments = safeToolArguments(msgs[i].ToolCalls[j].Arguments)
		}
	}
	return msgs
}

// safeToolArguments returns a string that parses as JSON.  An empty or null
// payload becomes an empty object, which is what a call with no arguments
// means; a valid object or array is passed through untouched.
func safeToolArguments(args string) string {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	if json.Valid([]byte(trimmed)) {
		if trimmed[0] == '{' || trimmed[0] == '[' {
			return args
		}
	}
	obj, err := json.Marshal(map[string]string{"_raw": args})
	if err != nil {
		return "{}"
	}
	return string(obj)
}

// RepairToolPairing makes a history acceptable to vendors that require each
// tool result to follow the assistant turn that declared its call id.
//
// It is deliberately provider-neutral: the gateway applies it once before any
// module sees a request, so every platform and every failover candidate gets the
// same valid history.  A result whose call is missing anywhere is dropped; an
// assistant call with no result is left in place because it is a valid
// mid-turn shape.  A history that is already well formed is returned unchanged.
func RepairToolPairing(msgs []Message) []Message {
	if len(msgs) < 2 {
		return msgs
	}
	owner := make(map[string]int, len(msgs))
	for i, m := range msgs {
		for _, tc := range m.ToolCalls {
			if strings.TrimSpace(tc.ID) != "" {
				owner[strings.TrimSpace(tc.ID)] = i
			}
		}
	}
	resultsFor := make(map[int][]Message)
	seenResult := make(map[string]bool)
	// First pass: find every result that owns a call and record it.  A call
	// with no result is malformed too, but only a later pass can safely drop it
	// without losing the assistant text it may carry.
	for _, m := range msgs {
		if strings.EqualFold(strings.TrimSpace(m.Role), "tool") {
			if id := strings.TrimSpace(m.ToolCallID); id != "" {
				if _, ok := owner[id]; ok {
					seenResult[id] = true
				}
			}
		}
	}
	handled := make([]bool, len(msgs))
	changed := false
	for j, m := range msgs {
		if !strings.EqualFold(strings.TrimSpace(m.Role), "tool") {
			continue
		}
		id := strings.TrimSpace(m.ToolCallID)
		at, ok := owner[id]
		if !ok {
			handled[j] = true
			changed = true
			continue
		}
		resultsFor[at] = append(resultsFor[at], m)
		handled[j] = true
		if at != j-1 {
			changed = true
		}
	}
	// Drop calls whose result does not exist anywhere in the history.  The
	// assistant text and reasoning on that turn are preserved.
	for i := range msgs {
		if len(msgs[i].ToolCalls) == 0 {
			continue
		}
		kept := make([]ToolCall, 0, len(msgs[i].ToolCalls))
		for _, tc := range msgs[i].ToolCalls {
			if seenResult[strings.TrimSpace(tc.ID)] {
				kept = append(kept, tc)
			} else {
				changed = true
			}
		}
		if len(kept) != len(msgs[i].ToolCalls) {
			msgs[i].ToolCalls = kept
		}
	}
	if !changed {
		return msgs
	}
	out := make([]Message, 0, len(msgs))
	for i, m := range msgs {
		if handled[i] {
			continue
		}
		if len(m.ToolCalls) > 0 || m.Content != "" || len(m.Parts) > 0 || m.Reasoning != "" || m.Role != "assistant" {
			out = append(out, m)
		}
		if owned := resultsFor[i]; len(owned) > 0 {
			out = append(out, owned...)
		}
	}
	return out
}
