package gateway

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// protocol-neutral orchestration
// ---------------------------------------------------------------------------
//
// Two wire protocols front this gateway: Chat Completions (/v1/chat/completions)
// and Responses (/v1/responses).  Almost all of what a request costs is the
// same for both -- resolve the model to a candidate list, apply conversation
// stickiness, hand the module the request, rotate accounts on a retryable
// refusal, walk the remaining platforms, and file exactly one usage row.
//
// Only two things differ: how the inbound body becomes a core.ChatRequest, and
// how a core.Stream is rendered back.  routeAndServe owns the former's
// consequence (the orchestration) and chatEmitter owns the latter (the
// encoding), so a third protocol would need a translator and an emitter and
// nothing else.

// routeInput is one inbound request after its wire body has been translated.
// Everything protocol-specific has already happened; what remains is the
// vendor-neutral orchestration.
type routeInput struct {
	// requested is the model name the caller wrote, before the registry
	// resolved it.  It decides two things the resolved id cannot: whether an
	// explicit platform was named (an explicit choice is answered honestly
	// instead of being hidden behind a failover), and what the resolver looks
	// the model up by.
	requested string
	// req is the translated request.  Its Model field is overwritten with each
	// resolved upstream id as candidates are tried.
	req *core.ChatRequest
	// stream reports whether the caller asked for a streamed response.
	stream bool
	// modelForWire is the model string the rendered frames echo back.  It is
	// the caller's own name, not the resolved upstream id, because that is what
	// a client echoes into its next turn.
	modelForWire string
	// rec and stat are this request's usage row and console row.  The caller
	// owns them -- it defers their completion -- so that a request refused
	// before routing is still recorded.
	rec  *UsageRecord
	stat *chatStat
	// emitter renders a successful stream in the caller's wire protocol.
	emitter chatEmitter
	// writeProtocolError, when non-nil, renders a pre-stream failure envelope
	// in the caller's wire protocol.  Nil selects the OpenAI-shaped default,
	// which is what the Chat and Responses endpoints use.  The Anthropic
	// endpoint supplies its own because Claude Code parses that shape.
	writeProtocolError func(w http.ResponseWriter, status int, typ, msg string)
	// toolNames maps each namespaced tool's flattened Chat name back to the
	// (namespace, name) pair it was declared as.  Only the Responses protocol
	// has namespaces, so every other caller leaves it nil.
	toolNames map[string]namespacedTool
}

// emitArgs is what an emitter needs to render one successful attempt.
type emitArgs struct {
	s       *server
	client  core.Client
	model   string
	stream  core.Stream
	hintCtx func() core.HintContext
	// toolNames is the table routeInput carries, passed through so a vendor's
	// flattened call can be put back into the pair Codex dispatches on.
	toolNames map[string]namespacedTool
	rec       *UsageRecord
	stat      *chatStat
}

// chatEmitter renders a core.Stream in one wire protocol.  The two methods
// mirror the caller's stream flag rather than taking it as an argument, so an
// emitter cannot disagree with the request about which shape was asked for.
//
// Error envelopes are deliberately not part of this interface: both protocols
// answer a refused request with the same OpenAI-shaped {"error":{…}} body, so
// the shared writeError / writeUpstreamError serve them both.  Only a failure
// that happens *after* a stream has opened is protocol-specific, and that is
// handled inside emitStream by the emitter that opened it.
type chatEmitter interface {
	emitStream(w http.ResponseWriter, r *http.Request, a emitArgs)
	emitBuffered(w http.ResponseWriter, r *http.Request, a emitArgs)
}

// chatWireEmitter renders OpenAI Chat Completions.  includeUsage is the
// caller's stream_options.include_usage, which that protocol makes opt-in; the
// Responses protocol always reports usage, so its emitter has no such field.
type chatWireEmitter struct{ includeUsage bool }

func (e chatWireEmitter) emitStream(w http.ResponseWriter, r *http.Request, a emitArgs) {
	a.s.streamSSE(w, r, a.client, a.model, a.stream, e.includeUsage, a.rec, a.stat, a.hintCtx)
}

func (e chatWireEmitter) emitBuffered(w http.ResponseWriter, r *http.Request, a emitArgs) {
	a.s.bufferCompletion(w, r, a.client, a.model, a.stream, a.rec, a.stat, a.hintCtx)
}

// protocolError renders a failure in the request's own wire protocol, falling
// back to the OpenAI-shaped envelope the Chat and Responses endpoints use.
func (in routeInput) protocolError(w http.ResponseWriter, status int, typ, msg string) {
	if in.writeProtocolError != nil {
		in.writeProtocolError(w, status, typ, msg)
		return
	}
	writeError(w, status, typ, msg)
}

// routeAndServe is the shared body of every completion endpoint: resolve the
// model, honour conversation stickiness, try the candidates in order, and hand
// the winning stream to the protocol's emitter.  Every failure writes the
// protocol's error envelope before returning.
func (s *server) routeAndServe(w http.ResponseWriter, r *http.Request, in routeInput) {
	s.routeAndServeMessages(w, r, in, writeError)
}

// routeAndServeMessages is routeAndServe with the failure envelope supplied by
// the caller, so a protocol whose error shape differs from OpenAI's still
// shares the entire routing, stickiness, failover and accounting path.
func (s *server) routeAndServeMessages(w http.ResponseWriter, r *http.Request, in routeInput, writeErr func(http.ResponseWriter, int, string, string)) {
	in.writeProtocolError = writeErr
	if !s.exposedModelAllowed(in.requested) {
		err := fmt.Errorf("model %q is not exposed by this gateway", in.requested)
		in.stat.status = http.StatusNotFound
		s.fail(in.rec, err)
		in.protocolError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	candidates, err := s.opts.Registry.ResolveCandidates(r.Context(), in.requested)
	if err != nil {
		in.stat.status = http.StatusNotFound
		s.fail(in.rec, err)
		in.protocolError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	virtualEffort := s.virtualEffortFor(in.requested, candidates)
	if virtualEffort != "" && normalizeEffort(in.rec.ReasoningEffort) == "" {
		// A virtual name (Auto/<id> or an equivalence group) may publish a
		// default thinking level.  Apply it before the pristine request is
		// copied so every candidate starts from the same caller-visible
		// default, while the usage row keeps the caller's own empty value.
		// A model that snaps unsupported levels down still does so on its own
		// candidate without changing the platform order.
		if in.req.Options == nil {
			in.req.Options = map[string]any{}
		}
		in.req.Options["reasoning_effort"] = virtualEffort
	}
	// A bare model may have several platforms.  Once one has served this
	// conversation, keep it first: trying the higher-priority sibling first
	// would make every turn pay for a platform that already failed.  The
	// binding never removes candidates, so a dead platform still fails over.
	conversationKey := core.ConversationKeyOf(in.req)
	if s.opts.Live != nil {
		s.sessionPlatforms.setTTL(s.opts.Live.Load().AffinityTTL)
	}
	if len(candidates) > 1 {
		if platform, ok := s.sessionPlatforms.resolve(conversationKey, func(name string) bool {
			for _, c := range candidates {
				if c.Client != nil && c.Client.Name() == name {
					// A platform that cannot serve right now must not keep a
					// sticky conversation pinned to it; the binding is only an
					// optimisation and the healthy candidate should win.
					if s.opts.Registry.ModelDegraded(c.Client.Name(), c.Model, time.Now()) {
						return false
					}
					return true
				}
			}
			return false
		}); ok {
			candidates = prioritizeStickyPlatform(candidates, platform)
		}
	}
	client, upstreamModel := candidates[0].Client, candidates[0].Model
	attemptCtx := withCatalogueExtra(r.Context(), candidates[0])
	in.rec.Candidate = 1
	// The caller's full history is the basis for every candidate.  Keeping a
	// pristine copy is what lets the failover chain re-trim for each platform
	// instead of inheriting the cuts made for a smaller-window sibling.
	baseReq := cloneChatRequest(in.req)
	maxTokensExplicit := in.req.MaxTokens != nil
	attemptReq, prepErr := s.prepareCandidateAttempt(attemptCtx, candidates[0], baseReq, in.rec, maxTokensExplicit)
	if prepErr != nil {
		// The newest turn alone does not fit this candidate.  That is a reason
		// to skip the candidate, not to refuse the whole request while a larger
		// sibling may still be able to serve it.
		hintCtx := s.hintContextFunc(attemptCtx, client, upstreamModel, baseReq.Messages)
		s.opts.Usage.RecordAttempt(UsageRecord{
			At:            time.Now(),
			StartedAt:     in.rec.StartedAt,
			Client:        client.Name(),
			SessionID:     in.rec.SessionID,
			Model:         client.Name() + "/" + upstreamModel,
			Candidate:     in.rec.Candidate,
			FailureReason: failureReason(prepErr),
			Failed:        true,
			Attempt:       true,
		})
		if len(candidates) > 1 && !hardPlatformPin(in.requested, candidates[0]) &&
			(s.opts.Guard == nil || !s.opts.Guard.IP.Active()) {
			s.opts.Logger.Printf("chat: platform %s model %s cannot fit the newest turn, trying the next platform", client.Name(), upstreamModel)
			var handled bool
			client, hintCtx, prepErr, handled = s.serveRemainingCandidates(w, r, in, candidates[1:], 2, maxTokensExplicit, baseReq)
			if handled {
				return
			}
		}
		status, _, _ := upstreamErrorShape(prepErr)
		in.stat.status = status
		s.fail(in.rec, prepErr)
		s.writeUpstreamError(w, client, hintCtx, prepErr)
		return
	}
	in.req = attemptReq
	// The row names the resolved module and model, which is what the console
	// reader needs in order to tell "the caller asked for something odd" apart
	// from "the module misbehaved".
	in.stat.model = in.rec.Model
	// Attribute only what the gateway can honestly know.  The account that
	// serves a successful request is chosen inside the module, so the gateway
	// must not guess it; a failure names its account through core.Failure.  A
	// realm is unambiguous only when the module advertises exactly one.
	if h, ok := core.HealthOf(client); ok && len(h.Realms) == 1 {
		for realm := range h.Realms {
			in.rec.Realm = realm
		}
	}

	// The module picks the credential, so only the module can name it.  The
	// gateway hands over a slot, calls Chat, and reads what the module wrote;
	// an empty slot means "the module did not say", which is recorded as
	// unknown rather than guessed at.
	// Assembled before the call so a failure can be explained with the request
	// that caused it rather than with the error text alone.
	hintCtx := s.hintContextFunc(attemptCtx, client, upstreamModel, in.req.Messages)

	var servedBy string
	in.req.ServedBy = &servedBy

	stream, err := s.openStreamRecorded(attemptCtx, client, in.req, in.rec)
	if err != nil {
		// A failure usually names its own account through core.Failure.  When it
		// does not, the slot still holds the last credential the module tried,
		// which is strictly better evidence than nothing.
		in.rec.Account = core.ErrorAccountID(err)
		if in.rec.Account == "" {
			in.rec.Account = servedBy
		}
		in.stat.uid = in.rec.Account

		f, classified := core.AsFailure(err)
		retryable := classified && core.Retryable(f.Kind)
		if retryable {
			ev := s.opts.Registry.NoteModelFailure(client.Name(), upstreamModel, time.Now())
			s.publishPlatformAlert(client.Name(), upstreamModel, in.rec.Account, ev)
		}
		skippable := candidateUnavailable(err) || (!hardPlatformPin(in.requested, candidates[0]) && modelRefusal(err))
		if (retryable || skippable) && len(candidates) > 1 && (s.opts.Guard == nil || !s.opts.Guard.IP.Active()) {
			s.opts.Usage.RecordAttempt(UsageRecord{
				At:                  time.Now(),
				StartedAt:           in.rec.StartedAt,
				Client:              client.Name(),
				Realm:               in.rec.Realm,
				Account:             in.rec.Account,
				SessionID:           in.rec.SessionID,
				ReasoningEffort:     in.rec.ReasoningEffort,
				SentReasoningEffort: in.rec.SentReasoningEffort,
				Model:               client.Name() + "/" + upstreamModel,
				Candidate:           in.rec.Candidate,
				FailureReason:       failureReason(err),
				Failed:              true,
				Attempt:             true,
				Trimmed:             in.rec.Trimmed,
				TrimDropped:         in.rec.TrimDropped,
				TrimmedFromTokens:   in.rec.TrimmedFromTokens,
				TrimmedToTokens:     in.rec.TrimmedToTokens,
				ContextWindow:       in.rec.ContextWindow,
			})
			if skippable {
				s.opts.Registry.NoteModelUnavailable(client.Name(), upstreamModel, time.Now())
			}
			s.opts.Logger.Printf("chat: platform %s model %s failed (%v), trying the next platform", client.Name(), upstreamModel, err)
			var handled bool
			client, hintCtx, err, handled = s.serveRemainingCandidates(w, r, in, candidates[1:], 2, maxTokensExplicit, baseReq)
			if handled {
				return
			}
		}
		status, _, _ := upstreamErrorShape(err)
		in.stat.status = status
		s.fail(in.rec, err)
		s.writeUpstreamError(w, client, hintCtx, err)
		return
	}
	// A success names nothing in the error path, so this is the only place the
	// serving account can be learned — and both the usage ledger's per-account
	// grouping and the console row are wrong without it.
	// A vendor may answer an exhausted account with HTTP 200 and the refusal
	// inside the stream.  Nothing has reached the caller yet, so reading the
	// first frame here is free: a classified retryable verdict closes that
	// stream and hands the request to the next platform instead of the caller.
	probe, probeErr := probeFirstFrame(stream)
	if probeErr != nil {
		stream.Close()
		in.rec.Account = core.ErrorAccountID(probeErr)
		if in.rec.Account == "" {
			in.rec.Account = servedBy
		}
		in.stat.uid = in.rec.Account
		f, classified := core.AsFailure(probeErr)
		retryable := classified && core.Retryable(f.Kind)
		if retryable {
			ev := s.opts.Registry.NoteModelFailure(client.Name(), upstreamModel, time.Now())
			s.publishPlatformAlert(client.Name(), upstreamModel, in.rec.Account, ev)
		}
		if (retryable || candidateUnavailable(probeErr)) && len(candidates) > 1 && (s.opts.Guard == nil || !s.opts.Guard.IP.Active()) {
			s.noteFailedCandidate(in.rec, client, upstreamModel, probeErr)
			s.opts.Logger.Printf("chat: platform %s model %s failed inside stream (%v), trying the next platform", client.Name(), upstreamModel, probeErr)
			var handled bool
			client, hintCtx, probeErr, handled = s.serveRemainingCandidates(w, r, in, candidates[1:], 2, maxTokensExplicit, baseReq)
			if handled {
				return
			}
		}
		status, _, _ := upstreamErrorShape(probeErr)
		in.stat.status = status
		s.fail(in.rec, probeErr)
		s.writeUpstreamError(w, client, hintCtx, probeErr)
		return
	}
	stream = withProbedFrame(stream, probe)

	if servedBy != "" {
		in.rec.Account = servedBy
		in.stat.uid = servedBy
	}
	s.opts.Registry.NoteModelSuccess(client.Name(), upstreamModel, time.Now())
	s.sessionPlatforms.bindModel(conversationKey, client.Name(), upstreamModel)
	stream = wrapStickyFailure(stream, client, conversationKey)
	// A turn that ends with "stop" and nothing to act on is the shape that
	// silently closes an agent loop; one more attempt is spent on it before the
	// caller is told the turn is over.  See deadturn.go.
	stream = wrapDeadTurnRetry(stream, in.req, func() (core.Stream, error) {
		next, err := s.openStreamRecorded(attemptCtx, client, in.req, in.rec)
		if err != nil {
			return nil, err
		}
		return wrapStickyFailure(next, client, conversationKey), nil
	}, s.opts.Logger.Printf)
	defer stream.Close()

	a := emitArgs{s: s, client: client, model: in.modelForWire, stream: stream, hintCtx: hintCtx, toolNames: in.toolNames, rec: in.rec, stat: in.stat}
	if in.stream {
		in.emitter.emitStream(w, r, a)
		return
	}
	in.emitter.emitBuffered(w, r, a)
}

// stickyFailureStream forgets a conversation binding after an opened stream
// fails. A partial response cannot be retried safely, but the next turn must
// be allowed to select a different account instead of returning to the one
// that just broke.
type stickyFailureStream struct {
	core.Stream
	binder core.ConversationBinder
	key    string
	once   sync.Once
}

func wrapStickyFailure(stream core.Stream, client core.Client, key string) core.Stream {
	if stream == nil || key == "" {
		return stream
	}
	binder, ok := core.AsConversationBinder(client)
	if !ok {
		return stream
	}
	return &stickyFailureStream{Stream: stream, binder: binder, key: key}
}

func (s *stickyFailureStream) forget() {
	s.once.Do(func() { s.binder.UnbindConversation(s.key) })
}

func (s *stickyFailureStream) Recv() (core.Event, error) {
	ev, err := s.Stream.Recv()
	if ev.Type == core.EventError || (err != nil && !errors.Is(err, io.EOF)) {
		s.forget()
	}
	return ev, err
}
