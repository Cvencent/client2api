package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

// ---------------------------------------------------------------------------
// Context window guard.
//
// A vendor that refuses an oversized prompt says so numerically:
//
//	prompt is too long: 100001 tokens > 100000 maximum
//
// That "100000 maximum" is the most useful number in the exchange: it is the
// model's real serving window, measured, for this exact model, at the exact
// moment the published catalogue turned out to be wrong.
//
// Three mechanisms, in the order they matter:
//
//  1. Trim.  Before the module is called, the assembled input is measured
//     against the model's window and, when it does not fit, the oldest turns
//     are dropped.  The request then succeeds instead of being refused.  Every
//     trim is recorded on the usage row, so nothing is ever dropped silently.
//  2. Learn.  When a refusal still happens -- the estimate was too generous,
//     or the vendor's real limit is below what the catalogue published -- the
//     number the vendor reported is recorded, so /v1/models stops advertising
//     the inflated figure and every downstream client that sizes its own
//     context stops over-packing in the first place.
//  3. Never guess.  Every mechanism is gated on actually knowing the window.
//     A model whose limit this gateway cannot determine is forwarded untouched,
//     because silently rewriting a request that would have succeeded is a far
//     worse failure than one honest 400.
//
// The trim keeps the system prompt and the newest turns and drops from the old
// end, which is where the stale context is.  The final turn is pinned: an answer
// without the question being asked right now is not a degraded answer, it is a
// different answer.

// promptBudgetSafety is the fraction of the input budget the guard aims for.
// The estimator is a heuristic, so the guard leaves headroom rather than
// trimming right up to the last token: a request that fits is worth much more
// than the handful of tokens this holds back.
const promptBudgetSafety = 0.92

// promptBudgetFloor is the smallest input budget the guard will enforce, in
// tokens.  Below this a model is small enough that trimming to fit would strip
// most of the conversation, so the honest answer is to let the caller decide.
const promptBudgetFloor = 4096

// promptOutputCap bounds how much of the window the guard reserves for the
// response, so a caller asking for a huge max_tokens does not shrink its own
// input budget without limit.
const promptOutputCap = 32 * 1024

// promptFixedOverhead is the non-message part of a request.  Vendors prepend
// their own system preamble and per-request framing that is never visible here,
// so the budget reserves a fixed allowance rather than pretending the messages
// are all of it.
const promptFixedOverhead = 512

// promptBudget is the number of input tokens a request for this model may use.
func (s *server) promptBudget(ctx context.Context, client core.Client, model string, maxOutput int) int64 {
	window := s.effectiveContextWindow(ctx, client, model)
	if window <= 0 {
		return 0
	}
	reserve := promptFixedOverhead
	if maxOutput > 0 {
		if maxOutput > promptOutputCap {
			maxOutput = promptOutputCap
		}
		reserve += maxOutput
	}
	budget := window - int64(reserve)
	if budget <= 0 {
		// The window cannot even hold the response the caller asked for.
		// Returning 0 disables trimming rather than producing a negative budget,
		// so the caller still gets the vendor's own refusal.
		return 0
	}
	budget = int64(float64(budget) * promptBudgetSafety)
	if budget < promptBudgetFloor {
		return 0
	}
	return budget
}

// leadingSystemTurns counts the system prompts at the head of a conversation.
//
// The count stops one short of the end so a conversation whose only turn is a
// system prompt still counts as nothing pinned: the caller must never end up
// with nothing left to answer.
func leadingSystemTurns(msgs []core.Message) int {
	n := 0
	for n < len(msgs)-1 && strings.EqualFold(strings.TrimSpace(msgs[n].Role), "system") {
		n++
	}
	return n
}

// trimReport describes what one trim did, for the log line and the usage row.
type trimReport struct {
	// DroppedMessages is how many of the oldest turns were removed.
	DroppedMessages int
	// BeforeTokens is the estimated input size before trimming.
	BeforeTokens int64
	// AfterTokens is the estimated input size after trimming.
	AfterTokens int64
	// Window is the window the request was measured against.
	Window int64
	// Impossible marks a request that cannot fit even with only the final turn
	// left.  The caller refuses it rather than answering a different question.
	Impossible bool
}

// applyContextBudget trims the request's history until the input fits the
// budget, and reports what it did.
//
// It returns ok=false and changes nothing when the window is unknown, when the
// request already fits, or when even the final turn alone cannot fit; the report
// distinguishes those cases.
func applyContextBudget(req *core.ChatRequest, budget int64) (trimReport, bool) {
	if req == nil || budget <= 0 || len(req.Messages) == 0 {
		return trimReport{}, false
	}
	estimated := int64(estimateRequestTokens(req))
	if estimated <= budget {
		return trimReport{}, false
	}

	// Two ends of the conversation are pinned, and neither is stale history:
	//
	//   - the leading system messages, which are the task itself.  Dropping them
	//     does not shorten the conversation, it changes what the model is being
	//     asked to do.
	//   - the final turn, which is the question being asked right now.  An answer
	//     without it is not a degraded answer, it is a different answer.
	//
	// Everything between them is droppable, oldest first, because that is where
	// the accumulated stale context lives.
	pinned := leadingSystemTurns(req.Messages)
	keepFrom := pinned
	for keepFrom < len(req.Messages)-1 {
		keepFrom++
		if int64(estimateMessagesTokens(req.Messages[keepFrom:])) <= budget {
			break
		}
	}
	if keepFrom >= len(req.Messages)-1 {
		// Everything except the pinned turns is still too big.  Cutting into what
		// is left would answer a different question, so the request is refused
		// locally and the caller learns exactly why.
		return trimReport{Impossible: true, BeforeTokens: estimated}, false
	}

	// The kept window is the pinned head, the droppable middle that survived, and
	// the pinned tail -- so the result is assembled explicitly rather than being a
	// single suffix, which would take the system prompts with it.
	dropped := keepFrom - pinned
	kept := make([]core.Message, 0, len(req.Messages)-dropped)
	kept = append(kept, req.Messages[:pinned]...)
	kept = append(kept, req.Messages[keepFrom:]...)
	req.Messages = kept
	return trimReport{
		DroppedMessages: dropped,
		BeforeTokens:    estimated,
		AfterTokens:     int64(estimateRequestTokens(req)),
	}, true
}

// ---------------------------------------------------------------------------
// Learning the real window from a refusal
// ---------------------------------------------------------------------------

// tooLongPatterns find a vendor's own limit in an upstream refusal.
//
// Each vendor words this differently and the gateway sees whatever the module
// put in the error string, so the patterns are anchored on the "N tokens" plus a
// comparison shape rather than on one vendor's phrasing.  A pattern only counts
// when it yields a plausible positive limit; anything else is treated as "the
// vendor did not say", which leaves the request failing exactly as it does today
// rather than inventing a limit.
var tooLongPatterns = []*regexp.Regexp{
	// "prompt is too long: 100001 tokens > 100000 maximum"
	regexp.MustCompile(`(?i)\b(\d[\d,_]*)\s*tokens?\b[^0-9]{0,40}?[>>=]{1,2}\s*(\d[\d,_]*)`),
	// "maximum context length is 100000 tokens"
	regexp.MustCompile(`(?i)max(?:imum)?[^0-9]{0,30}?(\d[\d,_]*)\s*tokens?`),
	// "exceeds the model's 100000 token limit"
	regexp.MustCompile(`(?i)exceed[^0-9]{0,30}?(\d[\d,_]*)\s*tokens?\s*(?:limit|window|context)`),
	// "context length 100000 exceeded"
	regexp.MustCompile(`(?i)context[^0-9]{0,20}?(?:length|window|limit)[^0-9]{0,20}?(\d[\d,_]*)[^0-9]{0,20}?exceed`),
}

// parseLimitFromMessage extracts a model's real context window from an upstream
// refusal message.
//
// It returns 0 when the message carries no usable number, which is the common
// case across modules that have no such vocabulary: 0 means "nothing was
// learned", never "the limit is zero".
func parseLimitFromMessage(msg string) int64 {
	lower := strings.ToLower(strings.TrimSpace(msg))
	if lower == "" || !mentionsSize(lower) {
		return 0
	}
	for _, re := range tooLongPatterns {
		m := re.FindStringSubmatch(lower)
		if m == nil {
			continue
		}
		// With two capture groups the second is the limit the request was
		// compared against; with one, the only number is the limit itself.
		if n, ok := parseTokenCount(m[len(m)-1]); ok {
			return n
		}
	}
	return 0
}

// mentionsSize reports whether a refusal is plausibly about prompt size at all.
// Kept separate from the patterns so the gate stays readable and the patterns
// stay about shape.
func mentionsSize(lower string) bool {
	for _, marker := range []string{
		"too long", "too large", "too big", "exceed", "exceeds", "exceeded",
		"maximum", "context", "token", "length", "window",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// parseTokenCount reads a possibly-separated integer ("104,857", "1_048_576").
func parseTokenCount(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	// Some locales write "104 857" with a narrow space.  Drop the separators only
	// when they are unambiguously grouping marks.
	s = strings.NewReplacer(",", "", "_", "", " ", "").Replace(s)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n < 1000 {
		// A "window" under a thousand tokens is not a serving limit; it is almost
		// always a different number that happened to be nearby.  Refusing to learn
		// from it is much better than shrinking a model on the strength of one
		// confusing sentence.
		return 0, false
	}
	return n, true
}

// learnContextWindowFromFailure records the window a vendor reported in a
// refusal, so later requests and every downstream client's own context
// management are sized correctly.
//
// A limit is only ever lowered towards what the vendor said, never raised: one
// confusing sentence must not inflate a model to a window it does not have, and
// the failure this exists to fix is specifically an over-reported window.
func (s *server) learnContextWindowFromFailure(ctx context.Context, client core.Client, model string, err error) int64 {
	if err == nil || client == nil || model == "" {
		return 0
	}
	limit := parseLimitFromMessage(err.Error())
	if limit <= 0 {
		return 0
	}
	if learned := s.learnedWindows.get(client.Name(), model); learned > 0 && learned <= limit {
		// We already know a window this size or smaller.  Do not grow it on the
		// strength of a sentence that may not be about this model at all.
		return learned
	}
	if s.opts.ModelOverrides != nil {
		if o, ok := s.opts.ModelOverrides.Get(client.Name(), model); ok && o.ContextLength > 0 {
			// The operator already said what this model's window is.  Record the
			// measurement anyway -- it is useful evidence -- but never let it
			// override the decision.
			s.learnedWindows.put(client.Name(), model, limit)
			return o.ContextLength
		}
	}
	published := s.publishedContextWindow(ctx, client, model)
	if published > 0 && limit >= published {
		// The vendor did not contradict us, so there is nothing to learn.  This
		// also covers a limit equal to the one we already advertise, which would
		// otherwise rewrite the file for no reason.
		return limit
	}
	if s.learnedWindows.put(client.Name(), model, limit) {
		s.opts.Logger.Printf(
			"context: %s/%s refused a prompt that needs more room than the %d tokens advertised; the vendor's own limit was %d, recorded it",
			client.Name(), model, published, limit)
	}
	return limit
}

// publishedContextWindow is the window the catalogue currently advertises for a
// model, ignoring anything this gateway has already learned.
//
// The module's own Extra has to be consulted here, not just the static table.
// The vendor's number is the one /v1/models actually publishes, so it is the
// one a request has to be judged against; resolving with an empty Extra would
// report "unknown" for every model whose window comes from the vendor, and the
// guard would then never trim anything at all.
func (s *server) publishedContextWindow(ctx context.Context, client core.Client, model string) int64 {
	if client == nil || model == "" {
		return 0
	}
	if s.opts.ModelOverrides != nil {
		if o, ok := s.opts.ModelOverrides.Get(client.Name(), model); ok && o.ContextLength > 0 {
			return o.ContextLength
		}
	}
	if models, err := client.Models(ctx); err == nil {
		for _, m := range models {
			if m.ID != model {
				continue
			}
			if extra := modelmeta.FromExtra(m.Extra, modelmeta.KeysCanonical, ""); extra.ContextLength > 0 {
				return extra.ContextLength
			}
			break
		}
	}
	return s.resolvedModelMeta(client, model, nil).ContextLength
}

// effectiveContextWindow is the window to judge a request against: whatever the
// vendor has actually said, falling back to the published catalogue.
//
// A manual override outranks a learned limit.  The operator is the final
// authority for this deployment: if the window really did change and the vendor
// is now serving more than it used to, learning would otherwise silently undo
// the number a person typed and there would be no way back except editing the
// file by hand.
func (s *server) effectiveContextWindow(ctx context.Context, client core.Client, model string) int64 {
	if s.opts.ModelOverrides != nil && client != nil && model != "" {
		if o, ok := s.opts.ModelOverrides.Get(client.Name(), model); ok && o.ContextLength > 0 {
			return o.ContextLength
		}
	}
	if learned := s.learnedWindows.get(client.Name(), model); learned > 0 {
		return learned
	}
	return s.publishedContextWindow(ctx, client, model)
}

// ---------------------------------------------------------------------------
// Persisted learned limits
// ---------------------------------------------------------------------------

// learnedWindowStore remembers context windows the vendors themselves reported,
// keyed by (platform, model).
//
// It is deliberately a separate store from modelmeta.OverrideStore.  That one
// belongs to the operator: the panel renders it as the manual source, and a
// shareable -NoData bundle is expected to reflect deliberate choices.  A
// measurement this gateway took by itself would land there wearing a label it
// never earned and would then be exported to other installations as if a person
// had typed it.  Keeping them apart means the panel can always answer "who
// decided this number" honestly.
type learnedWindowStore struct {
	mu      sync.RWMutex
	path    string
	warnf   func(string, ...any)
	entries map[learnedWindowKey]int64
}

type learnedWindowKey struct {
	client string
	model  string
}

// learnedWindowDoc is the on-disk shape.  A version is written so a future
// format change is detected instead of misread.
type learnedWindowDoc struct {
	Version int                         `json:"version"`
	Windows map[string]map[string]int64 `json:"windows"`
}

// learnedWindowVersion is the current on-disk format.
const learnedWindowVersion = 1

// openLearnedWindowStore loads the store from path.  Every failure mode -- a
// missing file, an unreadable one, a corrupt one -- collapses to an empty store:
// this is a cache of measurements and must never be able to break a request.
func openLearnedWindowStore(path string, warnf func(string, ...any)) *learnedWindowStore {
	s := &learnedWindowStore{
		path:    strings.TrimSpace(path),
		warnf:   warnf,
		entries: map[learnedWindowKey]int64{},
	}
	if s.path == "" {
		return s
	}
	var doc learnedWindowDoc
	if err := core.ReadJSON(s.path, &doc); err != nil {
		// A missing file is the normal first-run case, not a problem to report.
		if s.warnf != nil && !errors.Is(err, os.ErrNotExist) {
			s.warnf("context: ignoring unreadable learned limits %s: %v", s.path, err)
		}
		return s
	}
	if doc.Version != 0 && doc.Version != learnedWindowVersion {
		if s.warnf != nil {
			s.warnf("context: ignoring learned limits %s with version %d", s.path, doc.Version)
		}
		return s
	}
	for client, models := range doc.Windows {
		for model, limit := range models {
			if limit > 0 {
				s.entries[learnedWindowKey{client: client, model: model}] = limit
			}
		}
	}
	return s
}

// get returns a learned window, or 0 when this model has never been refused for
// being oversized.
func (s *learnedWindowStore) get(client, model string) int64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entries[learnedWindowKey{client: client, model: model}]
}

// put records a window and persists it.  It reports whether the stored value
// changed, so a refusal that repeats the same number does not rewrite the file.
func (s *learnedWindowStore) put(client, model string, limit int64) bool {
	if s == nil || client == "" || model == "" || limit <= 0 {
		return false
	}
	s.mu.Lock()
	key := learnedWindowKey{client: client, model: model}
	if prev, ok := s.entries[key]; ok && prev == limit {
		s.mu.Unlock()
		return false
	}
	s.entries[key] = limit
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil && s.warnf != nil {
		s.warnf("context: cannot persist the learned limit for %s/%s: %v", client, model, err)
	}
	return true
}

// snapshot returns a copy of every learned limit.
func (s *learnedWindowStore) snapshot() map[string]map[string]int64 {
	if s == nil {
		return map[string]map[string]int64{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

// snapshotLocked is snapshot without taking the lock.  Callers hold s.mu.
func (s *learnedWindowStore) snapshotLocked() map[string]map[string]int64 {
	out := map[string]map[string]int64{}
	for key, limit := range s.entries {
		models := out[key.client]
		if models == nil {
			models = map[string]int64{}
			out[key.client] = models
		}
		models[key.model] = limit
	}
	return out
}

// saveLocked writes the document atomically.  Callers hold s.mu.
func (s *learnedWindowStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	doc := learnedWindowDoc{Version: learnedWindowVersion, Windows: s.snapshotLocked()}
	return core.WriteJSONAtomic(s.path, doc)
}

// ---------------------------------------------------------------------------
// The request-path entry point
// ---------------------------------------------------------------------------

// guardRequestContext trims a request to its model's window, in place, and
// records what it did on the usage row.
//
// It is the one place that mutates a request for size, so it owns the two rules
// that keep that safe: it never touches a request whose window is unknown, and
// it never trims a request that already fits.
func (s *server) guardRequestContext(ctx context.Context, req *core.ChatRequest, client core.Client, model string, maxOutput int, rec *UsageRecord) (trimReport, bool) {
	budget := s.promptBudget(ctx, client, model, maxOutput)
	if budget <= 0 {
		// The window is unknown, or too small to trim against.  Forwarding the
		// request untouched is the only safe answer.
		return trimReport{}, false
	}
	report, trimmed := applyContextBudget(req, budget)
	if trimmed && rec != nil {
		rec.Trimmed = true
		rec.TrimDropped = report.DroppedMessages
		rec.TrimmedFromTokens = report.BeforeTokens
		rec.TrimmedToTokens = report.AfterTokens
		rec.ContextWindow = s.effectiveContextWindow(ctx, client, model)
	}
	return report, trimmed
}

// contextWindowRefusal is the error the gateway returns when it knows a request
// cannot fit and has deliberately chosen not to shorten it further.
//
// It is the same 400 with the same machine-readable code the vendor's own
// refusal already produced, so nothing downstream has to learn a new shape --
// but the message now carries the limit, which is the piece a caller needs in
// order to decide what to compact.
func contextWindowRefusal(client core.Client, model string, limit, estimated int64) error {
	msg := "the assembled request is larger than this model can serve and the gateway will not shorten it further"
	if limit > 0 && estimated > 0 {
		msg = fmt.Sprintf("prompt is too long: about %d tokens > %d maximum", estimated, limit)
	}
	return core.Fail(client.Name(), "", core.FailureContextWindow, 400,
		errors.New(msg+"; reduce the conversation history or switch to a model with a larger context window"))
}
