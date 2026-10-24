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

// probeEnabled reports whether the probe-before-trim behaviour is on.
//
// The live snapshot is authoritative when the panel is wired: an operator can
// turn the behaviour off from the settings page and have it apply to the next
// request, without a restart.  The startup option is the fallback for embedders
// that never build a live holder.
func (s *server) probeEnabled() bool {
	if s.opts.Live != nil {
		return s.opts.Live.Load().ProbeBeforeTrim
	}
	return s.opts.ProbeBeforeTrim
}

// probeAdjustedBudget is the trim budget after the model's accepted floor is
// taken into account.
//
// A probe that the vendor served proves the estimator was wrong about that
// size: the bytes were servable.  Leaving the budget where it was would trim
// the very request the probe just vindicated, so the floor becomes the budget.
// It is an observation, not a declaration, which is why it is used directly
// rather than being scaled by the safety margin that guards a vendor's claimed
// window.
//
// A manual override still wins.  The operator's number is the final authority
// for this deployment, and a measurement must not quietly route around it.
func (s *server) probeAdjustedBudget(ctx context.Context, client core.Client, model string, maxOutput int, limit int64) int64 {
	budget := s.promptBudget(ctx, client, model, maxOutput, limit)
	if s.probes == nil || client == nil || model == "" {
		return budget
	}
	rec, ok := s.probes.get(client.Name(), model)
	if !ok || rec.Outcome != probeOutcomeAccepted || rec.EstimateTokens <= 0 {
		return budget
	}
	floor := rec.EstimateTokens
	if s.opts.ModelOverrides != nil {
		if o, ok := s.opts.ModelOverrides.Get(client.Name(), model); ok && o.ContextLength > 0 {
			if floor > o.ContextLength {
				floor = o.ContextLength
			}
		}
	}
	if floor > budget {
		return floor
	}
	return budget
}

// probeVerdict is the guard's decision for one request before it is sent.
type probeVerdict int

const (
	// probeProceed means the normal budget logic owns this request.
	probeProceed probeVerdict = iota
	// probeForwardWhole means the vendor should decide: the estimate exceeds
	// the budget, but this (platform, model) has never been tested.
	probeForwardWhole
	// probeTrimExceeded means the request is too far over to be worth a probe.
	probeTrimExceeded
)

// probeDecision decides whether to trust the estimator or the vendor for this
// request.
//
// The order is deliberate.  An unknown window leaves the request untouched,
// which is the guard's oldest rule.  A request inside the budget needs no
// decision.  A model that has already been probed is settled by that verdict
// forever -- accepted means the recorded floor is the budget, refused means the
// normal window path applies.  Everything else is a model this gateway has
// never tested, and the vendor -- not the estimator -- decides whether it fits.
// There is deliberately no "too far over to bother" cutoff: the estimator is
// exactly the thing that cannot be trusted here, so any size it rejects is
// still worth one vendor round trip before the guard shortens the caller.
func (s *server) probeDecision(ctx context.Context, req *core.ChatRequest, client core.Client, model string, maxOutput int, limit int64) probeVerdict {
	if req == nil || client == nil || len(req.Messages) == 0 {
		return probeProceed
	}
	budget := s.probeAdjustedBudget(ctx, client, model, maxOutput, limit)
	if budget <= 0 {
		// The window is unknown: forward untouched, exactly as before.
		return probeProceed
	}
	estimate := int64(estimateRequestTokens(req))
	if estimate <= budget {
		return probeProceed
	}
	if s.probes != nil {
		if rec, ok := s.probes.get(client.Name(), model); ok {
			if !rec.Probed {
				// An explicit clear: the operator asked to re-verify this
				// model, so it is probed again despite any published window.
				return probeForwardWhole
			}
			if rec.Outcome == probeOutcomeAccepted {
				// The accepted floor is already folded into the budget this
				// decision was made against, so reaching here means the request
				// is above the proven size: trim it.
				return probeTrimExceeded
			}
			// Already refused: the lease on probing this model is spent.
			return probeProceed
		}
	}
	// A published window is not proof: the whole reason this path exists is
	// that the catalogue can be wrong in the caller's favour.  So a model with
	// a known window but no verdict is still worth one vendor round trip.  The
	// models page shows such a model as "已探测" because a number exists, but
	// the first request that actually exceeds the budget still verifies it.
	return probeForwardWhole
}

// requestContextWindow is the window a single request is judged against: the
// configured/vendor window, capped by any per-request test limit the caller
// supplied.  The limit is only ever a cap; it never widens a model.
func (s *server) requestContextWindow(ctx context.Context, client core.Client, model string, limit int64) int64 {
	window := s.effectiveContextWindow(ctx, client, model)
	if limit > 0 && (window <= 0 || limit < window) {
		return limit
	}
	return window
}

// promptBudget is the number of input tokens a request for this model may use.
func (s *server) promptBudget(ctx context.Context, client core.Client, model string, maxOutput int, limit int64) int64 {
	window := s.requestContextWindow(ctx, client, model, limit)
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
	// The budget is expressed in this gateway's estimate units, because that is
	// what applyContextBudget measures the request in.  A vendor whose
	// tokenizer reads fewer tokens per estimated token can hold proportionally
	// more of them, so the vendor-space budget is divided by how many vendor
	// tokens one estimated token is worth.  A model that has never been
	// measured reports 1 and this is a no-op, exactly as before.
	if factor := s.estimates.factor(client.Name(), model); factor > 0 && factor != 1 {
		budget = int64(float64(budget) / factor)
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

// isToolMessage reports whether a turn is a tool result.  Vendors validate that
// each one answers a tool call the assistant made, so keeping one without that
// assistant turn turns a recoverable request into a malformed one.
func isToolMessage(m core.Message) bool {
	return strings.EqualFold(strings.TrimSpace(m.Role), "tool")
}

// balancedTailStart returns the index at which the newest call-balanced suffix
// begins: the last message, extended backwards over a run of tool results and
// the assistant turn that issued the calls they answer.
//
// A tool result with no assistant call ahead of it is not a shorter
// conversation, it is a malformed one.  Vendors answer that with a bad-request
// refusal -- workbuddy's code 11133 -- rather than the oversized-prompt one the
// trim was trying to fix, so a trim that leaves one behind converts "too long"
// into "bad parameters" and the caller can never recover by resending.
func balancedTailStart(msgs []core.Message) int {
	last := len(msgs) - 1
	if last <= 0 || !isToolMessage(msgs[last]) {
		return last
	}
	start := last
	for start > 0 && isToolMessage(msgs[start-1]) {
		start--
	}
	if start > 0 {
		// The assistant turn that issued the calls these results answer.
		start--
	}
	return start
}

// applyContextBudget trims the request's history until the input fits the
// budget, and reports what it did.
//
// It returns ok=false and changes nothing when the window is unknown, when the
// request already fits, or when even the final turn alone cannot fit; the report
// distinguishes those cases.
//
// Two things are pinned and neither is stale history: the leading system
// messages, which are the task, and the newest call-balanced tail, which is the
// question being asked right now.  Everything between them is dropped oldest
// first.  The kept window is assembled so it can never begin with a tool result
// whose assistant turn was dropped: that is a malformed history, and vendors
// reject it as a bad request instead of an oversized prompt.
func applyContextBudget(req *core.ChatRequest, budget int64) (trimReport, bool) {
	if req == nil || budget <= 0 || len(req.Messages) == 0 {
		return trimReport{}, false
	}
	estimated := int64(estimateRequestTokens(req))
	if estimated <= budget {
		return trimReport{}, false
	}

	pinned := leadingSystemTurns(req.Messages)
	tail := balancedTailStart(req.Messages)
	if tail < pinned {
		tail = pinned
	}
	// Everything that is not history: the conversation envelope and the tool
	// declarations.  The vendor measures them as part of the request, so the
	// fit check has to count them too, or the trim lands above the budget it
	// was given and the retry is refused for the same reason.
	overhead := estimated - int64(estimateMessagesTokens(req.Messages))
	head := int64(estimateMessagesTokens(req.Messages[:pinned]))

	keepFrom := -1
	for from := pinned; from <= tail; from++ {
		if isToolMessage(req.Messages[from]) {
			// A result without the assistant call ahead of it is exactly the
			// malformed shape this loop exists to avoid.
			continue
		}
		if overhead+head+int64(estimateMessagesTokens(req.Messages[from:])) <= budget {
			keepFrom = from
			break
		}
	}
	if keepFrom < 0 {
		// Even the newest call-balanced tail cannot fit.  Cutting into it would
		// answer a different question, so the request is refused locally and the
		// caller learns exactly why.
		return trimReport{Impossible: true, BeforeTokens: estimated}, false
	}

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
// put in the error string, so the patterns are anchored on the "N tokens" plus
// a comparison shape rather than on one vendor's phrasing.  A pattern only
// counts when it yields a plausible positive limit; anything else is treated as
// "the vendor did not say", which leaves the request failing exactly as it does
// today rather than inventing a limit.
// The vendor's own measurement of the refused request is read separately, by
// measuredSizePatterns: litellm -- which fronts several of the platforms this
// gateway routes to -- writes the limit FIRST and the measurement second
// ("maximum context length is 1048576 tokens. However, you requested 1263369
// tokens"), the opposite of the "N tokens > M maximum" shape, so one pass over
// the text cannot assume an order.
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

// measuredSizePatterns find the size the vendor measured THIS request at, when
// the refusal names it.  They are tried independently of the limit patterns
// because the two numbers arrive in either order; a refusal that names only
// its window keeps Observed at 0, which means "the vendor did not say" rather
// than a fabricated measurement.
var measuredSizePatterns = []*regexp.Regexp{
	// litellm: "However, you requested 1263369 tokens (1163369 in the messages, ...)"
	regexp.MustCompile(`(?i)(?:you\s+)?requested\s+(\d[\d,_]*)\s*tokens?`),
	// "1427021 tokens > 1048576 maximum" -- the comparison form, measurement first
	regexp.MustCompile(`(?i)\b(\d[\d,_]*)\s*tokens?\s*[>>=]{1,2}`),
}

// wireUnicodeEscapeRE matches the JSON escapes a vendor's own error body may
// carry verbatim: workbuddy's 11115 body arrives with ">" written as "\u003e",
// and a parser that only knows the raw form reads every one of those refusals
// as "the vendor said nothing".
var wireUnicodeEscapeRE = regexp.MustCompile(`(?i)\\u([0-9a-f]{4})`)

// unescapeWireEscapes decodes \uXXXX sequences in an upstream message, so the
// size patterns can match text the vendor escaped on the wire.
func unescapeWireEscapes(s string) string {
	if !strings.Contains(s, `\u`) {
		return s
	}
	return wireUnicodeEscapeRE.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.ParseUint(m[2:], 16, 32)
		if err != nil {
			return m
		}
		return string(rune(n))
	})
}

// refusalSizes is what a vendor's refusal measured and what it compared that
// measurement against: the "1427021 tokens > 1048576 maximum" pair.
//
// Observed is 0 when the vendor named only its window; Limit is 0 when the
// message carries no usable number at all.  A zero always means "the vendor
// did not say", never "zero tokens".
type refusalSizes struct {
	Observed int64
	Limit    int64
}

// parseRefusalSizes extracts both numbers a vendor's refusal can carry from an
// upstream error string.
func parseRefusalSizes(msg string) refusalSizes {
	lower := strings.ToLower(strings.TrimSpace(unescapeWireEscapes(msg)))
	if lower == "" || !mentionsSize(lower) {
		return refusalSizes{}
	}

	// The limit and the measured size are found independently: vendors write
	// them in either order (litellm puts the limit first, workbuddy puts the
	// measurement first), and each may be absent on its own.
	var sizes refusalSizes
	for _, re := range tooLongPatterns {
		m := re.FindStringSubmatch(lower)
		if m == nil {
			continue
		}
		// With two capture groups the second is the limit; with one, the only
		// number is the limit itself.
		if limit, ok := parseTokenCount(m[len(m)-1]); ok {
			sizes.Limit = limit
			break
		}
	}
	for _, re := range measuredSizePatterns {
		m := re.FindStringSubmatch(lower)
		if m == nil {
			continue
		}
		if observed, ok := parseTokenCount(m[1]); ok {
			sizes.Observed = observed
			break
		}
	}
	// A "measurement" that is not actually larger than the limit is one of
	// the limit's own numbers seen twice, not a measurement.  Dropping it
	// keeps Observed meaning exactly what it says.
	if sizes.Limit > 0 && sizes.Observed <= sizes.Limit {
		sizes.Observed = 0
	}
	return sizes
}

// parseLimitFromMessage extracts a model's real context window from an upstream
// refusal message.
//
// It returns 0 when the message carries no usable number, which is the common
// case across modules that have no such vocabulary: 0 means "nothing was
// learned", never "the limit is zero".
func parseLimitFromMessage(msg string) int64 {
	return parseRefusalSizes(msg).Limit
}

// contextRefusalPhrases are the wordings a prompt-size refusal uses when it
// names no numbers at all.
//
// They are deliberately whole phrases rather than the bare words "context" or
// "length": those appear in unrelated rejections (a malformed context field, a
// bad length parameter), and promoting one of them would tell the caller to
// compact a request that has nothing to do with size.  Adding a phrase here is
// how a newly observed vendor wording becomes a compaction trigger; the pattern
// based, numbered refusals are handled separately by parseRefusalSizes.
//
// OpenCode Zen is the shape this exists for: "Your request resulted in more
// tokens than this model's maximum context length allows. Please reduce the
// length of the messages or run compaction."  It carries no token count, so the
// gateway can neither learn a window from it nor size a trim against it -- but
// it must still reach the caller as context_window_exceeded, because that is the
// one code a Responses client triggers conversation compaction on.
var contextRefusalPhrases = []string{
	// English
	"maximum context length",
	"context length allows",
	"context length exceeded",
	"context_length_exceeded",
	"exceeds the maximum context",
	"prompt is too long",
	"prompt too long",
	"input is too long",
	"too many tokens",
	"tokens than this model",
	"reduce the length of the messages",
	"run compaction",
	// The Chinese wordings the same family of vendors uses.  Loomy's own module
	// classifies its refusal already; this recogniser is the fallback for any
	// module that does not, so the phrasing lives here too.
	"内容太长",
	"上下文过长",
	"请精简输入",
	"开启新对话",
}

// looksLikeContextRefusal reports whether a refusal is about prompt size and,
// when the vendor named one, the limit it compared against.
//
// ok=false means the message is not a size refusal at all.  ok=true with
// limit==0 means "the vendor said too long but named no number": that is still a
// context-window verdict for the caller, it just carries nothing the guard can
// learn from.
func looksLikeContextRefusal(msg string) (limit int64, ok bool) {
	text := strings.ToLower(strings.TrimSpace(unescapeWireEscapes(msg)))
	if text == "" {
		return 0, false
	}
	if limit := parseLimitFromMessage(text); limit > 0 {
		return limit, true
	}
	for _, phrase := range contextRefusalPhrases {
		if strings.Contains(text, phrase) {
			return 0, true
		}
	}
	return 0, false
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

// normalizeContextRefusal promotes a size refusal that a module failed to
// classify into core.FailureContextWindow.
//
// The gateway owns the wire vocabulary, and "the caller should compact" is a
// decision it is entitled to make from the vendor's own sentence even when the
// module only saw an unremarkable 400.  Without this, a module that has no
// context pattern answers invalid_request, which no client compacts on, and the
// same oversized body is re-sent forever.
//
// A refusal that already carries a classified kind is never rewritten: a module
// that said "rate limited" or "auth" knows something the wording does not.
func normalizeContextRefusal(err error) error {
	if err == nil {
		return nil
	}
	if f, ok := core.AsFailure(err); ok && f.Kind != core.FailureOther {
		return err
	}
	limit, ok := looksLikeContextRefusal(err.Error())
	if !ok {
		return err
	}
	_ = limit // a limit, when the vendor named one, is read separately by the guard
	var (
		client, account string
		status          int
	)
	if f, ok := core.AsFailure(err); ok {
		client, account, status = f.Client, f.Account, f.Status
	}
	if status <= 0 {
		status = 400
	}
	return core.Fail(client, account, core.FailureContextWindow, status, err)
}

// learnWindowFloor is the fraction of the largest prompt this gateway has
// actually had SERVED for a model that a learned limit may not go below.
//
// It exists because a vendor's refusal number is not always the model's real
// serving window.  A refusal can name a limit that applies to one condition --
// a single request, one session state, one backend -- while the same model
// serves far larger prompts moments later.  Learning 100000 from one refusal
// when the same model was observed serving 220000 tokens an hour earlier would
// permanently shrink the model to less than half of what it can do, and the
// guard would then trim healthy requests for the rest of the process's life.
//
// The floor only ever raises the learned number back up to observed reality.  It
// is deliberately not a second source of truth: it can make the guard more
// permissive than one refusal suggested, and never less permissive than either
// the vendor or the operator said.
const learnWindowFloorNumerator = 4
const learnWindowFloorDenominator = 5

// observedPromptFloor returns the largest input size this gateway has had the
// vendor successfully serve for a model, or 0 when it has no evidence.
//
// It reads the usage ledger's recent ring, which is already in memory and is
// reloaded from disk at startup, so the floor survives a restart without this
// package keeping any state of its own.
//
// Failed rows are excluded on purpose: a refusal never reached the model.
func (s *server) observedPromptFloor(clientName, model string) int64 {
	if s.opts.Usage == nil {
		return 0
	}
	want := clientName + "/" + model
	var max int64
	for _, rec := range s.opts.Usage.Snapshot() {
		if rec.Failed || rec.PromptTokens <= 0 {
			continue
		}
		if rec.Client+"/"+rec.Model != want {
			continue
		}
		if int64(rec.PromptTokens) > max {
			max = int64(rec.PromptTokens)
		}
	}
	return max
}

// applyLearnFloor raises a vendor-reported limit to the fraction of the largest
// prompt actually served for this model.
//
// ok=false means there was no evidence to apply and the caller's number stands.
func (s *server) applyLearnFloor(client core.Client, model string, limit int64) (int64, bool) {
	observed := s.observedPromptFloor(client.Name(), model)
	if observed <= 0 {
		return limit, false
	}
	floor := observed * learnWindowFloorNumerator / learnWindowFloorDenominator
	if limit >= floor {
		return limit, false
	}
	return floor, true
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
	// Before believing the refusal, check it against what this model was actually
	// observed to serve.  A refusal number that is far below a prompt the vendor
	// has already answered is describing a narrower condition than "this model's
	// window", and learning it verbatim would shrink the model permanently.
	if floored, raised := s.applyLearnFloor(client, model, limit); raised {
		s.opts.Logger.Printf(
			"context: %s/%s reported a %d token limit, but this gateway has had it serve a %d token prompt; learning %d instead",
			client.Name(), model, limit, s.observedPromptFloor(client.Name(), model), floored)
		limit = floored
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

// catalogueExtraContextKey carries the metadata the resolver already read for
// one candidate. It is request-scoped on purpose: the catalogue can change at
// any time, and a cross-request cache would serve stale context windows.
type catalogueExtraContextKey struct{}

type catalogueExtraContext struct {
	extra map[string]any
}

func withCatalogueExtra(ctx context.Context, candidate core.Candidate) context.Context {
	if !candidate.Catalogue {
		return ctx
	}
	return context.WithValue(ctx, catalogueExtraContextKey{}, catalogueExtraContext{extra: candidate.Extra})
}

func catalogueExtraFromContext(ctx context.Context) (map[string]any, bool) {
	v, ok := ctx.Value(catalogueExtraContextKey{}).(catalogueExtraContext)
	if !ok {
		return nil, false
	}
	return v.extra, true
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
	if extra, ok := catalogueExtraFromContext(ctx); ok {
		return s.resolvedModelMeta(client, model, extra).ContextLength
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

// measuredRefusalMaxOvershoot caps how far past the window a vendor's own
// measurement may be before this gateway stops treating it as a conversation
// that can be trimmed to fit.  A request tens of times over the limit is more
// likely to have been measured under a different condition than to be a
// history worth cutting down to a sliver for.
const measuredRefusalMaxOvershoot = 8

// retryBudgetForMeasuredRefusal turns the vendor's own two numbers from a
// refusal into the estimator-sized budget a retry should be trimmed to.
//
// The proactive guard could not have used these numbers: it only has the
// heuristic estimate, and a refusal happens when that estimate read lower than
// the vendor's tokenizer did.  The refusal measures the real request, so the
// ratio between the vendor's count and the gateway's estimate says how much of
// the history the vendor's window can actually hold.
func retryBudgetForMeasuredRefusal(req *core.ChatRequest, observed, limit int64, maxOutput int) int64 {
	if req == nil || observed <= 0 || limit <= 0 || observed <= limit {
		return 0
	}
	if observed > limit*measuredRefusalMaxOvershoot {
		return 0
	}
	estimated := int64(estimateRequestTokens(req))
	if estimated <= 0 {
		return 0
	}
	reserve := int64(promptFixedOverhead)
	if maxOutput > promptOutputCap {
		maxOutput = promptOutputCap
	}
	if maxOutput > 0 {
		reserve += int64(maxOutput)
	}
	room := limit - reserve
	if room <= 0 {
		return 0
	}
	room = int64(float64(room) * promptBudgetSafety)
	budget := estimated * room / observed
	if budget < promptBudgetFloor || budget >= estimated {
		return 0
	}
	return budget
}

// unmeasuredRefusalRetryDivisor is how much of the advertised window a retry
// keeps when the vendor refused the prompt as oversized but named no numbers.
//
// This is a retry size, never a measurement: nothing is persisted and the
// model's advertised window is left exactly as it was.  The vendor said "too
// long" about a request this gateway sized to fit, which means the advertised
// window is optimistic by at least that much, so the retry aims well inside it.
// Halving is the conservative end of "well inside" -- the newest call-balanced
// tail is always kept, so what survives is the question, not a shrunken
// transcript that answers something else.
const unmeasuredRefusalRetryDivisor = 2

// retryBudgetForUnmeasuredRefusal builds a retry budget from the window the
// gateway already advertises, for a refusal that carries no numbers at all.
//
// Some vendors answer an oversized prompt with prose and nothing else -- Loomy's
// "...内容太长了，请精简输入或开启新对话" names neither the size it measured nor the
// limit it compared against.  There is nothing to learn from that and no
// proportion to keep, so the guard cannot size the trim the way
// retryBudgetForMeasuredRefusal does.  It can still make one conservative
// attempt: trim to a fraction of the advertised window and retry.  A caller who
// is served wins; a caller who is refused again gets the vendor's own 400 back,
// exactly as they would have without the retry.
func retryBudgetForUnmeasuredRefusal(req *core.ChatRequest, window int64, maxOutput int) int64 {
	if req == nil || window <= 0 {
		return 0
	}
	estimated := int64(estimateRequestTokens(req))
	if estimated <= 0 {
		return 0
	}
	reserve := int64(promptFixedOverhead)
	if maxOutput > promptOutputCap {
		maxOutput = promptOutputCap
	}
	if maxOutput > 0 {
		reserve += int64(maxOutput)
	}
	room := window/unmeasuredRefusalRetryDivisor - reserve
	if room <= 0 {
		return 0
	}
	room = int64(float64(room) * promptBudgetSafety)
	if room < promptBudgetFloor || room >= estimated {
		return 0
	}
	return room
}

// ---------------------------------------------------------------------------
// The request-path entry point
// ---------------------------------------------------------------------------

// prepareCandidateAttempt builds one candidate's request from the caller's
// pristine history and trims it to that candidate's own context window.
//
// The full history, not a previously trimmed one, is the basis for every
// candidate: a platform with a larger window must receive everything the
// caller sent, even when an earlier, smaller sibling already had to drop turns.
// A candidate whose newest turn cannot fit is reported as an error so the
// router can skip it and keep walking the user's priority order.
func (s *server) prepareCandidateAttempt(ctx context.Context, candidate core.Candidate, requestedModel string, base *core.ChatRequest, rec *UsageRecord, maxTokensExplicit bool) (*core.ChatRequest, error) {
	client, model := candidate.Client, candidate.Model
	req := cloneChatRequest(base)
	if req == nil {
		req = &core.ChatRequest{}
	}
	// Tool-call/result pairing is a vendor-independent history invariant, not a
	// quirk of one platform.  Repair it once on the pristine history before
	// per-candidate trimming so every module (and every failover sibling) sees
	// the same valid ordering.  This also prevents a trim boundary from leaving
	// a result whose call has already disappeared.
	req.Messages = core.RepairToolPairing(req.Messages)
	// The arguments a vendor parses as JSON are the other half of that
	// invariant.  A replayed call can carry text that is not JSON -- observed
	// in real transcripts as a missing quote or a truncated payload -- and a
	// strict relay answers it with HTTP 400 for the whole request.  Normalise
	// it here, on the same pristine history, so every candidate is safe.
	req.Messages = core.RepairToolArguments(req.Messages)
	req.Model = model
	req.FreeModel = candidate.Free

	if rec != nil {
		// Each candidate owns its own trim verdict. Without this reset, a trim
		// performed for a failed candidate would be copied onto whichever
		// candidate finally served the request.
		rec.Trimmed = false
		rec.TrimDropped = 0
		rec.TrimmedFromTokens = 0
		rec.TrimmedToTokens = 0
		rec.ContextWindow = 0
		rec.Client = client.Name()
		rec.Model = client.Name() + "/" + model
		rec.Realm = ""
		if h, ok := core.HealthOf(client); ok && len(h.Realms) == 1 {
			for realm := range h.Realms {
				rec.Realm = realm
			}
		}
	}

	// A caller that left the output budget to us gets the number this platform
	// publishes for the model. An explicit max_tokens is the caller's decision
	// and is never second-guessed.
	if !maxTokensExplicit {
		req.MaxTokens = nil
		if n, ok := s.resolveMaxOutputTokens(ctx, client, model); ok {
			req.MaxTokens = &n
		}
	}
	maxOutput := 0
	if req.MaxTokens != nil {
		maxOutput = *req.MaxTokens
	}
	limit := req.ContextWindowLimit

	report, trimmed := s.guardRequestContext(ctx, req, client, model, requestedModel, maxOutput, rec)
	if report.Impossible {
		window := s.requestContextWindow(ctx, client, model, limit)
		if rec != nil {
			rec.ContextWindow = window
		}
		s.opts.Logger.Printf(
			"context: refusing %s/%s: even the newest turn alone is about %d tokens, over the %d-token window",
			client.Name(), model, report.BeforeTokens, window)
		return nil, contextWindowRefusal(client, model, window, report.BeforeTokens)
	}
	if trimmed {
		window := s.effectiveContextWindow(ctx, client, model)
		s.opts.Logger.Printf(
			"context: trimmed %d oldest message(s) for %s/%s to fit %d tokens (about %d -> %d)",
			report.DroppedMessages, client.Name(), model, window,
			report.BeforeTokens, report.AfterTokens)
	}

	// Thinking policy is per candidate: xhigh is the top tier Codex sends, but
	// some vendors call their real top tier max. Cap alignment is opt-in per
	// model and only changes the caller's own top tier.
	if rec != nil {
		requestedEffort := rec.ReasoningEffort
		if requestedEffort == "" {
			requestedEffort = optionEffort(req.Options)
		}
		sentEffort := s.applyEffortPolicy(ctx, client, model, requestedModel, req, requestedEffort)
		rec.SentReasoningEffort = ""
		if sentEffort != "" && sentEffort != rec.ReasoningEffort {
			rec.SentReasoningEffort = sentEffort
		}
	}
	if rec != nil {
		// The usage row carries this gateway's own estimate of the request it
		// actually sent, so the vendor's reported prompt_tokens can later be
		// compared against it for the same bytes.  See estimate_calibration.go.
		rec.EstimatedPromptTokens = int64(estimateRequestTokens(req))
	}
	// This is a gateway-only sizing instruction, not part of the upstream wire
	// request. Clear it after the guard has used it so no module can mistake it
	// for a field it should forward.
	req.ContextWindowLimit = 0
	return req, nil
}

// guardRequestContext trims a request to its model's window, in place, and
// records what it did on the usage row.
//
// It is the one place that mutates a request for size, so it owns the two rules
// that keep that safe: it never touches a request whose window is unknown, and
// it never trims a request that already fits.
func (s *server) guardRequestContext(ctx context.Context, req *core.ChatRequest, client core.Client, model, requestedModel string, maxOutput int, rec *UsageRecord) (trimReport, bool) {
	limit := int64(0)
	if req != nil {
		limit = req.ContextWindowLimit
	}
	budget := s.probeAdjustedBudget(ctx, client, model, maxOutput, limit)
	if budget <= 0 {
		// The window is unknown, or too small to trim against.  Forwarding the
		// request untouched is the only safe answer.
		return trimReport{}, false
	}
	// Before trusting the estimator enough to shorten the caller's history,
	// give the vendor the first word on a model this gateway has never tested.
	// The vendor has the final say on its own window; the estimator is only a
	// heuristic, and when it runs high it cuts turns that would have been
	// served.  One accepted probe settles the question for this (platform,
	// model) permanently, so this is a one-time round trip, not a per-request
	// tax.
	if s.probeEnabled() && rec != nil {
		switch s.probeDecision(ctx, req, client, model, maxOutput, limit) {
		case probeForwardWhole:
			rec.Probed = true
			rec.ProbedFromTokens = int64(estimateRequestTokens(req))
			return trimReport{}, false
		case probeTrimExceeded:
			// Too far over to be worth a probe; fall through to the trim.
		default:
			// probeProceed: the normal budget path decides.
		}
	}
	report, trimmed := applyContextBudget(req, budget)
	if trimmed && rec != nil {
		rec.Trimmed = true
		rec.TrimDropped = report.DroppedMessages
		rec.TrimmedFromTokens = report.BeforeTokens
		rec.TrimmedToTokens = report.AfterTokens
		rec.ContextWindow = s.requestContextWindow(ctx, client, model, limit)
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
