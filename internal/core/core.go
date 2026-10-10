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

// ErrPlatformExhausted reports that a module has already tried every usable
// account on its platform for this request.  The gateway must then move to the
// next platform instead of re-running the same platform's account rotation.
// Modules that do not return it keep the previous retry behaviour.
var ErrPlatformExhausted = errors.New("every usable account on this platform failed")

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
