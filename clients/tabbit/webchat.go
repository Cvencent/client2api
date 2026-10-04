package tabbit

// This file owns the web transport's chat path: room, run, join, deltas.
//
// One API request is one vendor room.  The module opens a fresh room, opens its
// event stream, submits the run and re-frames the streamed deltas into core
// events; the room is then dropped, because the vendor retires it after every
// run ("room_reset_required", reason "terminal_snapshot_available").  That is
// also why a multi-turn caller has its history flattened into the single
// content field the submit endpoint accepts.
//
// Order matters: the join must be opened BEFORE the run is submitted, because
// the stream is what carries the answer and the vendor only replays a run that
// already has a listener.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Prompt
// ---------------------------------------------------------------------------

// webPrompt renders the caller's messages as the one content string the vendor
// accepts.  A single user turn is sent verbatim, which is the common case and
// keeps the caller's own formatting intact; anything longer becomes a labelled
// transcript, because the vendor has no message array to put roles in.
//
// The submit endpoint takes a single content string, so there is nowhere to put
// an image on this transport.  Silently dropping the part is the worst outcome
// available: the model answers as if it had been asked about something it never
// saw, and the caller has no way to tell the answer was fabricated.  The
// sidecar transport (openai.go) does carry image_url parts, so a request that
// mixes text and an image is answerable there.
func webPrompt(req *core.ChatRequest) (string, error) {
	if req == nil {
		return "", fmt.Errorf("%w: nil request", core.ErrUnsupported)
	}
	for _, m := range req.Messages {
		if hasNonTextPart(m) {
			return "", fmt.Errorf("%w: the Tabbit web transport takes one text field and cannot carry an image; use the tabbit sidecar transport, or send the image as text", core.ErrUnsupported)
		}
	}
	texts := make([]string, 0, len(req.Messages))
	roles := make([]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		text := messageText(m)
		if text == "" {
			continue
		}
		texts = append(texts, text)
		roles = append(roles, m.Role)
	}
	if len(texts) == 0 {
		return "", fmt.Errorf("%w: the request carried no text", core.ErrUnsupported)
	}
	if len(texts) == 1 && (roles[0] == "" || strings.EqualFold(roles[0], "user")) {
		return texts[0], nil
	}
	var b strings.Builder
	for i, text := range texts {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(webRoleLabel(roles[i]))
		b.WriteString(": ")
		b.WriteString(text)
	}
	return b.String(), nil
}

// messageText flattens one message, tolerating the array form.
func messageText(m core.Message) string {
	if s := strings.TrimSpace(m.Content); s != "" {
		return s
	}
	var b strings.Builder
	for _, p := range m.Parts {
		if t := strings.TrimSpace(p.Text); t != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t)
		}
	}
	return strings.TrimSpace(b.String())
}

// hasNonTextPart reports whether a message carries a part this transport cannot
// render.  Only the two OpenAI part types exist today; an unknown type counts,
// because dropping it is the failure mode this function exists to prevent.
func hasNonTextPart(m core.Message) bool {
	for _, p := range m.Parts {
		if p.Type != "" && !strings.EqualFold(p.Type, "text") {
			return true
		}
	}
	return false
}

// webRoleLabel names a role the way a transcript would.
func webRoleLabel(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant":
		return "Assistant"
	case "system":
		return "System"
	case "tool":
		return "Tool"
	default:
		return "User"
	}
}

// webHTMLEscaper keeps caller text out of the vendor's HTML field.
var webHTMLEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// webHTML renders the prompt as the paragraph HTML the official client sends.
func webHTML(prompt string) string {
	escaped := webHTMLEscaper.Replace(prompt)
	// Split on newlines the way the web client does: one paragraph per line.
	parts := strings.Split(escaped, "\n")
	for i, p := range parts {
		parts[i] = "<p>" + p + "</p>"
	}
	return strings.Join(parts, "")
}

// ---------------------------------------------------------------------------
// Submit and join
// ---------------------------------------------------------------------------

// webRunRequest is the body the official web client posts to start a run.
type webRunRequest struct {
	ClientRunID          string          `json:"client_run_id"`
	ClientMessageID      string          `json:"client_message_id"`
	AgentPlanModeEnabled bool            `json:"agent_plan_mode_enabled"`
	InputPayload         webInputPayload `json:"input_payload"`
}

type webInputPayload struct {
	TaskName        string         `json:"task_name"`
	Content         string         `json:"content"`
	ParentMessageID any            `json:"parent_message_id"`
	SelectedModel   []string       `json:"selected_model"`
	References      []any          `json:"references"`
	Metadatas       map[string]any `json:"metadatas"`
	PageInfoList    []any          `json:"page_info_list"`
	IsMobile        bool           `json:"is_mobile"`
	AgentMode       bool           `json:"agent_mode"`
}

// webSubmit queues one run in the room.  It returns the vendor's run id, which
// is only ever used for logging: the answer arrives on the join stream.
func (c *Client) webSubmit(ctx context.Context, wa webAuth, room, model, prompt string) (string, error) {
	body, err := json.Marshal(webRunRequest{
		ClientRunID:          runID(),
		ClientMessageID:      runID(),
		AgentPlanModeEnabled: true,
		InputPayload: webInputPayload{
			TaskName:        webTaskName,
			Content:         prompt,
			ParentMessageID: nil,
			SelectedModel:   []string{model},
			References:      []any{},
			Metadatas:       map[string]any{"html_content": webHTML(prompt)},
			PageInfoList:    []any{},
		},
	})
	if err != nil {
		return "", fmt.Errorf("building the Tabbit run body: %w", err)
	}
	path := fmt.Sprintf(webRunsPathFmt, url.PathEscape(room))
	resp, err := c.webDo(ctx, wa, http.MethodPost, path, nil, body, "application/json")
	if err != nil {
		return "", fmt.Errorf("submitting a Tabbit run: %w", err)
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, 1<<20)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", webError(wa, "POST "+path, resp.StatusCode, raw)
	}
	var env struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		// A 2xx whose body cannot be read is still a queued run: the stream is
		// what carries the answer, so this must not fail the request.
		return "", nil
	}
	return strings.TrimSpace(env.RunID), nil
}

// webJoin opens the room's event stream.
//
// The x-nonce / x-signature / x-timestamp triple is generated per call.  The
// vendor does not verify the signature, but the official client always sends
// the three headers, so this module does too.
func (c *Client) webJoin(ctx context.Context, wa webAuth, room string) (*http.Response, error) {
	body, err := json.Marshal(map[string]any{
		"page_instance_id": newUUID(),
		"device_id_hash":   deviceIDHash(wa.uid, room),
		"surface":          webSurface,
		"last_event_id":    nil,
	})
	if err != nil {
		return nil, fmt.Errorf("building the Tabbit join body: %w", err)
	}
	extra := http.Header{}
	extra.Set("Cache-Control", "no-cache")
	extra.Set("X-Nonce", randHex(32))
	extra.Set("X-Signature", newUUID())
	extra.Set("X-Timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))

	path := fmt.Sprintf(webJoinPathFmt, url.PathEscape(room))
	resp, err := c.webDoExtra(ctx, wa, http.MethodPost, path, nil, body, "text/event-stream", extra)
	if err != nil {
		return nil, fmt.Errorf("joining the Tabbit chat session: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw := readLimited(resp.Body, 1<<20)
		resp.Body.Close()
		return nil, webError(wa, "POST "+path, resp.StatusCode, raw)
	}
	if !isEventStream(resp.Header.Get("Content-Type")) {
		raw := readLimited(resp.Body, 1<<20)
		resp.Body.Close()
		return nil, fmt.Errorf("Tabbit answered %s with %q instead of an event stream: %s",
			path, resp.Header.Get("Content-Type"), truncate(upstreamErrorMessage(raw), 200))
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// The stream
// ---------------------------------------------------------------------------

// webEvent is the envelope every join frame carries.
type webEvent struct {
	EventID   string          `json:"event_id"`
	RoomID    string          `json:"room_id"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

// webStream re-frames the vendor's join SSE into core events.
//
// The vendor adds event types without warning (session_title_updated appeared
// mid-project), so every unknown type is ignored rather than treated as an
// error: the only events that end a stream are the terminal ones below.
type webStream struct {
	body   io.ReadCloser
	reader *sseReader
	cancel context.CancelFunc
	log    func(string, ...any)

	pending []core.Event
	started bool // our run has started; deltas before that are snapshot replay
	finish  string
	done    bool
	closed  bool
}

func newWebStream(body io.ReadCloser, cancel context.CancelFunc, log func(string, ...any)) *webStream {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &webStream{
		body:   body,
		reader: newSSEReader(body),
		cancel: cancel,
		log:    log,
	}
}

func (s *webStream) Recv() (core.Event, error) {
	if len(s.pending) > 0 {
		ev := s.pending[0]
		s.pending = s.pending[1:]
		return ev, nil
	}
	if s.done {
		return core.Event{}, io.EOF
	}
	for {
		frame, err := s.reader.next()
		if err != nil {
			s.done = true
			if errors.Is(err, io.EOF) {
				return core.Event{Type: core.EventDone, Finish: s.finishReason()}, nil
			}
			return core.Event{}, err
		}
		events, terminal := s.consume(frame)
		if terminal {
			s.done = true
		}
		if len(events) == 0 {
			if terminal {
				return core.Event{Type: core.EventDone, Finish: s.finishReason()}, nil
			}
			continue
		}
		if terminal {
			events = append(events, core.Event{Type: core.EventDone, Finish: s.finishReason()})
		}
		s.pending = events[1:]
		return events[0], nil
	}
}

func (s *webStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}

func (s *webStream) finishReason() string {
	if s.finish == "" {
		return "stop"
	}
	return s.finish
}

// consume turns one SSE frame into zero or more core events.  The second return
// value reports whether the stream is finished.
func (s *webStream) consume(frame sseFrame) ([]core.Event, bool) {
	data := strings.TrimSpace(frame.Data)
	if data == "" {
		return nil, false
	}
	if isDonePayload(data) {
		return nil, true
	}
	var env webEvent
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		// A frame this module cannot read is not a reason to kill a run that is
		// otherwise streaming; the terminal event still ends it.
		s.log("tabbit: ignoring an unreadable join frame: %v", err)
		return nil, false
	}
	typ := strings.TrimSpace(env.EventType)
	if typ == "" {
		typ = strings.TrimSpace(frame.Event)
	}
	s.log("tabbit: join frame type=%q chunk=%q", typ, webPayloadChunkType(env.Payload))
	switch typ {
	case "assistant_message_delta":
		if !s.started {
			// The snapshot replays earlier turns before our run begins.
			return nil, false
		}
		// A single frame can carry both channels: reasoning_content arrives in
		// an event_message_chunk/thinking frame, while the answer text arrives
		// in an assistant_message_delta.  They are separate frames in practice,
		// but reading both keys from one frame costs nothing and keeps a
		// vendor that merges them from losing the text.
		text := webDeltaText(env.Payload)
		reasoning := webDeltaReasoning(env.Payload)
		if text == "" && reasoning == "" {
			return nil, false
		}
		return []core.Event{{Type: core.EventDelta, Delta: text, Reasoning: reasoning}}, false
	case "event_message_chunk":
		// Reasoning-only frames.  The answer text does not come this way, so
		// this case exists purely to surface chunk_type=thinking; dropping it
		// loses the model's own explanation of its answer.
		if !s.started {
			return nil, false
		}
		reasoning := webDeltaReasoning(env.Payload)
		if reasoning == "" {
			return nil, false
		}
		return []core.Event{{Type: core.EventDelta, Reasoning: reasoning}}, false
	case "assistant_message_started", "run_started":
		s.started = true
		return nil, false
	case "run_completed":
		if status := webPayloadStatus(env.Payload); status != "" && !strings.EqualFold(status, "completed") {
			return []core.Event{{Type: core.EventError, Err: fmt.Errorf(
				"tabbit: the run ended as %s%s", status, webEventDetail(env.Payload))}}, true
		}
		return nil, true
	case "room_reset_required":
		// The vendor retires a room after every run.  The answer is already
		// complete by now, so this is normal, not a failure.
		return nil, true
	case "run_failed", "run_error", "error":
		return []core.Event{{Type: core.EventError, Err: fmt.Errorf(
			"tabbit: %s", webEventMessage(env.Payload))}}, true
	default:
		// snapshot_begin, message_history, active_runs_snapshot, snapshot_end,
		// run_queued, user_message_created, assistant_message_finished,
		// session_title_updated ... deliberately ignored.  thinking_finished and
		// message_chunk with an empty payload are the other end of a channel
		// that opened above; there is nothing to emit.
		return nil, false
	}
}

// webDeltaText pulls the increment out of a delta payload.  The vendor sends it
// in payload.delta and repeats it in payload.chunk_payload.content; the first
// non-empty one wins so an increment is never emitted twice.
func webDeltaText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var p struct {
		Delta        string `json:"delta"`
		ChunkPayload struct {
			Delta   string `json:"delta"`
			Content string `json:"content"`
		} `json:"chunk_payload"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	for _, candidate := range []string{p.Delta, p.ChunkPayload.Delta, p.ChunkPayload.Content} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

// webDeltaReasoning pulls the model's thinking out of a payload.  The vendor
// sends it under chunk_payload.reasoning_content; the top-level key is read as
// a fallback because the same envelope is reused across chunk types.
func webDeltaReasoning(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var p struct {
		ReasoningContent string `json:"reasoning_content"`
		ChunkPayload     struct {
			ReasoningContent string `json:"reasoning_content"`
		} `json:"chunk_payload"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	for _, candidate := range []string{p.ChunkPayload.ReasoningContent, p.ReasoningContent} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

// webPayloadChunkType names the sub-kind of a chunk frame, for the log line
// that records what the vendor sent.  Empty when the frame is not a chunk.
func webPayloadChunkType(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var p struct {
		ChunkType string `json:"chunk_type"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.ChunkType
}

// webPayloadStatus reads a run status out of a payload.
func webPayloadStatus(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var p struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return strings.TrimSpace(p.Status)
}

// webEventMessage is the vendor's own words for a failed event.
func webEventMessage(raw json.RawMessage) string {
	detail := webEventDetail(raw)
	if detail == "" {
		return "the run failed and the vendor sent no message"
	}
	return strings.TrimPrefix(detail, ": ")
}

// webEventDetail renders ": <error|message|detail>" for a payload that carries
// one, so a failure is never reported as "something went wrong".
func webEventDetail(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var p struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	for _, candidate := range []string{p.Error, p.Message, p.Detail, p.Reason} {
		if s := strings.TrimSpace(candidate); s != "" {
			return ": " + truncate(s, 300)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Entry points
// ---------------------------------------------------------------------------

// openWebStream runs one completion through web.tabbit.com.
func (c *Client) openWebStream(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", core.ErrUnsupported)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// The vendor takes display_name, which is the bare id this module stores.
	model := strings.TrimSpace(stripModelPrefix(req.Model, c.cfg.ModelPrefix))
	if model == "" {
		return nil, fmt.Errorf("%w: model is required", core.ErrUnsupported)
	}
	prompt, err := webPrompt(req)
	if err != nil {
		return nil, err
	}
	// resolveWebAuthFor, not resolveWebAuth: on this path a conversation
	// binding gets to choose among the stored sessions (affinity.go).  It binds
	// the chosen session here, before any network call, so two concurrent
	// requests for the same new conversation agree on one session instead of
	// racing onto two.
	wa, err := c.resolveWebAuthFor(conversationKey(req), model)
	if err != nil {
		return nil, err
	}
	// Name the credential for the gateway's usage ledger and console row.  The
	// session is settled here, before any network call, and it is the id the
	// account table reports (tabbit-web:<uid>).
	// Take the session's in-flight slot before the call.  A saturated session
	// is backpressure, not a session failure.
	if err := req.AcquireAccountSlot(wa.accountID); err != nil {
		return nil, err
	}
	core.NoteServedBy(req, wa.accountID)

	rctx := ctx
	cancel := context.CancelFunc(func() {})
	if d := c.cfg.Timeouts.requestBudget(); d > 0 {
		rctx, cancel = context.WithTimeout(ctx, d)
	}
	// The stream owns the cancellation once it exists.
	fail := func(err error) (core.Stream, error) {
		cancel()
		c.noteError(err.Error())
		c.noteWebFailure(wa, err)
		return nil, err
	}

	room, err := c.webCreateRoom(rctx, wa)
	if err != nil {
		return fail(err)
	}
	join, err := c.webJoin(rctx, wa, room)
	if err != nil {
		return fail(err)
	}
	if _, err := c.webSubmit(rctx, wa, room, model, prompt); err != nil {
		join.Body.Close()
		return fail(err)
	}
	c.noteWebSuccess(wa)
	return newWebStream(join.Body, cancel, c.deps.Log), nil
}

// webModels is Models() on the web transport.  Unlike the sidecar path it never
// invents a catalogue: the vendor's list is the list, and a failure is reported
// as a failure so the caller can see why.
func (c *Client) webModels(ctx context.Context) ([]core.Model, error) {
	if models, ok := c.cachedCatalog(); ok && len(models) > 0 {
		return models, nil
	}
	wa, err := c.resolveWebAuth()
	if err != nil {
		return nil, err
	}
	models, err := c.webFetchModels(ctx, wa)
	if err != nil {
		c.noteWebFailure(wa, err)
		return nil, err
	}
	if len(models) == 0 {
		return nil, errors.New("web.tabbit.com listed no models")
	}
	c.storeCatalog(models, time.Now())
	c.setDiscovered(wa.base)
	c.noteWebSuccess(wa)
	return models, nil
}

// webStatus is Status() on the web transport.  It is bounded by the status
// budget because the panel polls it: one catalogue call, nothing more.
func (c *Client) webStatus(ctx context.Context, st core.Status) core.Status {
	accounts := c.webAccounts()
	if len(accounts) == 0 {
		st.Detail = "no Tabbit web session is known: sign in inside the Tabbit browser, then press 导入凭据"
		st.Accounts = []core.AccountStatus{{
			ID:      "web-token",
			Label:   "web.tabbit.com cookie",
			Enabled: true,
			State:   epStateUnknown,
			Note:    "no credential has been imported yet",
		}}
		return st
	}

	probeCtx := ctx
	if d := c.cfg.Timeouts.statusBudget(); d > 0 {
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	usable := 0
	for _, ep := range accounts {
		acct := core.AccountStatus{ID: ep.ID, Label: endpointLabel(ep), Enabled: ep.Enabled}
		claims, _ := parseWebToken(ep.Token)
		expires := webTokenExpiry(claims)
		switch {
		case !ep.Enabled:
			acct.State = epStateUnknown
			acct.Note = "disabled by operator"
		case !expires.IsZero() && time.Now().After(expires):
			acct.State = epStateInvalid
			acct.Note = "the cookie expired at " + expires.UTC().Format(time.RFC3339) +
				"; press 导入凭据 after signing in again"
		default:
			if v, fresh := c.webVerified(ep.ID); fresh {
				// The panel polls Status on a loop and a finished chat stores its
				// own verdict, so a result younger than webVerdictTTL is reused
				// rather than spending the status budget on the catalogue again.
				acct.State = epStateReady
				acct.Note = fmt.Sprintf("cookie for %s; %d models from web.tabbit.com (checked %s ago)",
					shortUID(claims.Sub), v.models, time.Since(v.at).Round(time.Second))
				break
			}
			wa := c.webAuthFrom(ep.Token, endpointLabel(ep), epOriginPanel, ep.BaseURL)
			models, err := c.webFetchModels(probeCtx, wa)
			switch {
			case err == nil:
				c.storeWebVerdict(ep.ID, webVerdict{at: time.Now(), models: len(models)})
				acct.State = epStateReady
				acct.Note = fmt.Sprintf("cookie for %s; %d models from web.tabbit.com",
					shortUID(claims.Sub), len(models))
			case probeCtx.Err() != nil:
				// The status budget ran out.  That is a fact about the network,
				// not about the cookie, so fall back to the last real verdict
				// instead of reporting a state the account table does not share:
				// a row that reads green under a red badge is exactly the bug
				// this module used to have.
				if v, ok := c.webTrusted(ep.ID); ok {
					if v.err != "" {
						acct.State = epStateInvalid
						acct.Note = v.err
					} else {
						acct.State = epStateReady
						acct.Note = fmt.Sprintf("cookie for %s; last verified %s ago (%d models); the status probe ran out of budget",
							shortUID(claims.Sub), time.Since(v.at).Round(time.Second), v.models)
					}
					break
				}
				acct.State = epStateUnknown
				acct.Note = "web.tabbit.com did not answer within the status budget; press Test to check it with the longer budget"
			default:
				c.storeWebVerdict(ep.ID, webVerdict{
					at:        time.Now(),
					err:       core.Redact(truncate(err.Error(), 300)),
					transient: !webAuthRejection(err),
				})
				acct.State = epStateInvalid
				acct.Note = truncate(err.Error(), 200)
			}
		}
		if acct.State == epStateReady {
			usable++
		}
		st.Accounts = append(st.Accounts, acct)
	}
	st.Ready = usable > 0
	st.Detail = fmt.Sprintf("%d of %d Tabbit web session(s) usable", usable, len(accounts))
	if c.cfgErr != nil {
		st.Detail += "; config rejected: " + truncate(c.cfgErr.Error(), 120)
	}
	return st
}

// testWebAccount is TestAccount's web branch.  It reads the vendor's real
// catalogue and then runs a real completion, so OK means the cookie works end
// to end.  Every failure is a Result, never a Go error.
func (c *Client) testWebAccount(ctx context.Context, ep storedEndpoint) (res core.TestResult) {
	res = core.TestResult{AccountID: ep.ID}
	start := time.Now()
	defer func() { res.ElapsedMS = time.Since(start).Milliseconds() }()

	wa := c.webAuthFrom(ep.Token, endpointLabel(ep), epOriginPanel, ep.BaseURL)
	models, err := c.verifyWeb(ctx, wa)
	if err != nil {
		res.Error = truncate(err.Error(), 300)
		return res
	}
	if len(models) == 0 {
		res.Error = "web.tabbit.com answered but listed no models"
		return res
	}
	model := strings.TrimSpace(stripModelPrefix(models[0].ID, c.cfg.ModelPrefix))
	res.Model = model

	stream, err := c.openWebStream(ctx, &core.ChatRequest{
		Model:    model,
		Messages: []core.Message{{Role: "user", Content: testPrompt}},
	})
	if err != nil {
		res.Error = truncate(err.Error(), 300)
		return res
	}
	reply, err := drainTabbitStream(stream)
	if err != nil {
		res.Error = truncate(err.Error(), 300)
		return res
	}
	res.OK = true
	res.Reply = truncate(strings.TrimSpace(reply), 200)
	// The vendor's quota report is the other real thing this cookie can tell us,
	// and Test is the only place a second call is affordable: Status is polled,
	// so it must stay one call.  Best effort — the completion above decides OK.
	if u, uerr := c.webFetchUsage(ctx, wa); uerr == nil {
		res.Reply += " (" + usageNote(u) + ")"
	}
	return res
}

// noteWebSuccess records that a credential just worked, keeping the catalogue
// size the panel shows in the account note.
func (c *Client) noteWebSuccess(wa webAuth) {
	c.storeWebVerdict(wa.accountID, webVerdict{at: time.Now(), models: len(c.statusModels())})
}

// noteWebFailure records why a credential failed.  A rejection (401/403) makes
// auto mode fall back to the sidecar; a transport failure does not, because it
// says nothing about the cookie.
func (c *Client) noteWebFailure(wa webAuth, err error) {
	if err == nil {
		return
	}
	kind := core.FailureKindOf(err)
	status := 0
	if f, ok := core.AsFailure(err); ok {
		status = f.Status
	}
	c.storeWebVerdict(wa.accountID, webVerdict{
		at:        time.Now(),
		err:       core.Redact(truncate(err.Error(), 300)),
		transient: !webAuthRejection(err),
		kind:      kind,
		status:    status,
	})
}
