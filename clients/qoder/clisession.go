package qoder

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// clisession.go drives one Qoder CN agent-SDK worker over its stream-json
// protocol and exposes it as a core.Stream.
//
// Wire format (protocol_version 1.5.0, read out of the shipped worker and
// confirmed live):
//
//	in : {"type":"user","message":{"role":"user","content":[...]}}
//	out: {"type":"stream_event","event":{...}}   one per model delta
//	     {"type":"assistant","message":{...}}    the settled turn
//	     {"type":"result","subtype":...,"is_error":...,"errors":[...]}
//
// The inner stream_event objects are Anthropic-Messages shaped, which maps onto
// core.Event almost one for one:
//
//	content_block_delta/text_delta       -> EventDelta{Delta}
//	content_block_delta/thinking_delta   -> EventDelta{Reasoning}
//	content_block_start/tool_use         -> EventToolCall{ID,Name}
//	content_block_delta/input_json_delta -> EventToolCall{Arguments}
//	message_delta/usage                  -> EventUsage
//	result                               -> EventDone / EventError
//
// One process serves one conversation.  The worker owns a transcript, so two
// conversations sharing a process would bleed into each other; the pool in
// clipool.go keys on (account, conversation) for exactly that reason.

// ---------------------------------------------------------------------------
// wire types
// ---------------------------------------------------------------------------

// cliLine is one line of the worker's stream-json output.
type cliLine struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	Event   json.RawMessage `json:"event"`

	Result     string   `json:"result"`
	IsError    bool     `json:"is_error"`
	Errors     []string `json:"errors"`
	StopReason string   `json:"stop_reason"`

	// A queued turn reports itself instead of failing.  The free models are
	// heavily contended (thousands of requests waiting, up to an hour), so this
	// is a normal state and not an error.
	ModelKey         string `json:"model_key"`
	QueueCount       int    `json:"queue_count"`
	WaitTimeMS       int64  `json:"wait_time_ms"`
	QueueMaxWaitMS   int64  `json:"queue_max_wait_ms"`
	ServiceAvailable bool   `json:"service_available"`

	Usage *cliUsage `json:"usage"`
}

// cliUsage is the vendor's token ledger.  The free models report it as all
// zeroes (they are billed in credits, not tokens), so it is forwarded verbatim
// rather than used for accounting.
type cliUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CacheRead    int `json:"cache_read_input_tokens"`
	CacheWrite   int `json:"cache_creation_input_tokens"`
}

// cliEvent is the inner, Anthropic-shaped stream event.
type cliEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Usage *cliUsage `json:"usage"`
}

// ---------------------------------------------------------------------------
// translation
// ---------------------------------------------------------------------------

// eventsFromInner maps one inner stream event onto zero or more core.Events.  A
// single content block yields several events (a tool call announces its name
// once and then streams its arguments), so the result is a slice.
func eventsFromInner(ev cliEvent) []core.Event {
	switch ev.Type {
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text == "" {
				return nil
			}
			return []core.Event{{Type: core.EventDelta, Delta: ev.Delta.Text}}
		case "thinking_delta":
			if ev.Delta.Thinking == "" {
				return nil
			}
			return []core.Event{{Type: core.EventDelta, Reasoning: ev.Delta.Thinking}}
		case "input_json_delta":
			// Tool arguments arrive as JSON fragments; the gateway reassembles
			// them by index for every module alike.
			if ev.Delta.PartialJSON == "" {
				return nil
			}
			return []core.Event{{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{
				Index:     ev.Index,
				Arguments: ev.Delta.PartialJSON,
			}}}
		}
	case "content_block_start":
		if ev.ContentBlock.Type == "tool_use" {
			return []core.Event{{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{
				Index: ev.Index,
				ID:    ev.ContentBlock.ID,
				Name:  ev.ContentBlock.Name,
			}}}
		}
	case "message_delta":
		if ev.Usage != nil {
			return []core.Event{{Type: core.EventUsage, Usage: usageFromCLI(*ev.Usage)}}
		}
	}
	return nil
}

// usageFromCLI renders the vendor ledger in the gateway's vocabulary.
func usageFromCLI(u cliUsage) *core.Usage {
	return &core.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
		CachedTokens:     u.CacheRead,
	}
}

// finishFromCLI maps the vendor's stop reason onto the gateway's finish word.
func finishFromCLI(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "end_turn", "stop", "stop_sequence":
		return "stop"
	case "max_tokens", "length":
		return "length"
	case "tool_use", "tool_calls":
		return "tool_calls"
	case "":
		return ""
	default:
		return reason
	}
}

// cliAuthRefusal reports whether the worker's own words describe a missing or
// rejected credential rather than a transient problem.  The check is on the
// vendor's message because that is the only signal the protocol carries: the
// worker reports a signed-out turn as a successful result line with is_error
// set and "Not logged in" as the result text.
func cliAuthRefusal(msg string) bool {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "not logged in"),
		strings.Contains(m, "please run /login"),
		strings.Contains(m, "authentication") && strings.Contains(m, "fail"),
		strings.Contains(m, "登录") && strings.Contains(m, "失效"):
		return true
	}
	return false
}

// cliRefusalError classifies one refusal so the pool parks the right account.
func cliRefusalError(accountID, msg string) error {
	kind := core.FailureUpstream
	if cliAuthRefusal(msg) {
		kind = core.FailureAuth
	}
	return core.Fail(clientName, accountID, kind, 502, errors.New("qoder: "+msg))
}

// ---------------------------------------------------------------------------
// the worker process
// ---------------------------------------------------------------------------

// cliProc is one running worker bound to one config root.  It is reused across
// turns: starting it costs seconds, so paying that per request would dominate
// latency.
type cliProc struct {
	inst    cliInstall
	dir     string
	model   string
	effort  string
	account string

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan []byte
	werr    error
	started bool
	dead    bool
	lastUse time.Time
	gen     int

	// busy is the serial slot: capacity one, so a second turn on the same
	// conversation queues instead of being refused.  The worker's stdin
	// protocol processes one turn at a time, and a queued turn is exactly what
	// a caller retrying while the first answer streams should get.
	busy chan struct{}
}

// newCLIProc prepares (but does not start) a worker for one config root.
func newCLIProc(inst cliInstall, dir, account, model, reasoningEffort string) *cliProc {
	return &cliProc{
		inst:    inst,
		dir:     dir,
		account: account,
		model:   model,
		effort:  reasoningEffort,
		busy:    make(chan struct{}, 1),
	}
}

// cliArgs is the command line one worker is started with.
//
// --tools is passed an explicit empty list.  This bridge is a chat transport:
// the caller's own tool declarations are not forwarded (the CLI has no channel
// for them), and leaving the CLI's built-in filesystem and shell tools live
// would let a model run commands in the account's config directory for a
// request that never asked for it.
func (p *cliProc) cliArgs() []string {
	args := []string{
		"--config-dir", p.dir,
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--model", p.model,
		"--tools", "",
	}
	if p.effort != "" {
		args = append(args, "--reasoning-effort", p.effort)
	}
	return args
}

// alive reports whether the process is still usable without touching the
// filesystem or the child.
func (p *cliProc) alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started && !p.dead
}

// start launches the worker and its reader goroutine.  The caller holds no
// lock.  A process that is already running is left alone.
func (p *cliProc) start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started && !p.dead {
		return nil
	}
	// A previous incarnation has to be fully gone before its pipes are
	// replaced, or two children would share one account directory.
	if p.cmd != nil && p.cmd.Process != nil && !p.dead {
		killProcessTree(p.cmd.Process)
	}
	cmd := newCLICmd(p.inst, p.dir, p.cliArgs()...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("qoder: cli stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("qoder: cli stdout: %w", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("qoder: starting the Qoder CLI: %w", err)
	}
	p.cmd, p.stdin = cmd, stdin
	p.started, p.dead = true, false
	p.werr = nil
	p.gen++
	gen := p.gen
	lines := make(chan []byte, 256)
	p.lines = lines

	go func() {
		defer close(lines)
		scanErr := scanLines(stdout, func(line []byte) error {
			select {
			case lines <- line:
				return nil
			case <-time.After(30 * time.Second):
				// A consumer that stopped reading must not pin this
				// goroutine forever; the next turn restarts the worker.
				return io.EOF
			}
		})
		_ = cmd.Wait()
		p.mu.Lock()
		if p.gen == gen {
			p.werr = scanErr
			p.dead = true
		}
		p.mu.Unlock()
	}()
	return nil
}

// begin sends one user turn and returns the stream that reads its answer.
//
// The worker is serial: one turn at a time per process, which is what its
// stdin protocol allows.  A second turn waits for the first to settle, so a
// client that retries while the previous answer is still streaming queues
// rather than errors.
func (p *cliProc) begin(ctx context.Context, text string) (*cliTurn, error) {
	line, err := cliUserLine(text)
	if err != nil {
		return nil, err
	}
	select {
	case p.busy <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if err := p.start(); err != nil {
		p.finish()
		return nil, err
	}

	p.mu.Lock()
	stdin, dead := p.stdin, p.dead
	p.mu.Unlock()
	if stdin == nil || dead {
		p.finish()
		return nil, errors.New("qoder: the Qoder CLI is not running")
	}
	if _, err := stdin.Write(append(line, '\n')); err != nil {
		p.finish()
		p.kill()
		return nil, fmt.Errorf("qoder: writing to the Qoder CLI: %w", err)
	}
	return newCLITurn(ctx, p), nil
}

// finish releases the serial slot and records the idle clock.  It is safe to
// call more than once for one turn.
func (p *cliProc) finish() {
	select {
	case <-p.busy:
	default:
	}
	p.mu.Lock()
	p.lastUse = time.Now()
	p.mu.Unlock()
}

// kill terminates the worker and marks it for a restart on the next turn.  The
// serial slot is drained here too, so a caller blocked on begin is released.
func (p *cliProc) kill() {
	p.finish()
	p.mu.Lock()
	cmd, stdin := p.cmd, p.stdin
	p.dead = true
	p.cmd, p.stdin = nil, nil
	p.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		killProcessTree(cmd.Process)
	}
}

// stop terminates the worker.  It is safe to call more than once.
func (p *cliProc) stop() {
	p.kill()
}

// idleFor reports how long the worker has been unused.  A never-used worker is
// reported as idle since the epoch, which is what the pool wants when it sweeps.
func (p *cliProc) idleFor(now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.busy) > 0 {
		return 0
	}
	if p.lastUse.IsZero() {
		return now.Sub(time.Unix(0, 0))
	}
	return now.Sub(p.lastUse)
}

// noteUse records the turn start, so a long turn is not swept as idle.
func (p *cliProc) noteUse() {
	p.mu.Lock()
	p.lastUse = time.Now()
	p.mu.Unlock()
}

// markSettled records that the vendor's own result line arrived, which is what
// makes the worker reusable: only then is its stdout known to be drained.
func (t *cliTurn) markSettled() {
	t.mu.Lock()
	t.done = true
	t.mu.Unlock()
}

// settled reports whether the turn reached the vendor's own result line.
func (t *cliTurn) settled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

// ---------------------------------------------------------------------------
// the per-turn stream
// ---------------------------------------------------------------------------

// cliTurn turns one worker reply into a core.Stream.
type cliTurn struct {
	ctx      context.Context
	cancel   context.CancelFunc
	events   chan core.Event
	once     sync.Once
	proc     *cliProc
	finished chan struct{}

	mu   sync.Mutex
	done bool

	// queuedUntil is pushed forward by every model_queue_status line, so the
	// stall guard measures silence since the vendor last said "still waiting"
	// rather than since the turn began.  The vendor's own ceiling is an hour,
	// which is far past the default guard.
	queuedUntil time.Time
}

// noteQueued extends this turn's patience to the vendor's own queue ceiling.
func (t *cliTurn) noteQueued(waitMS, maxMS int64) {
	budget := maxMS
	if budget <= 0 {
		budget = waitMS
	}
	if budget <= 0 {
		budget = int64(cliLineStall / time.Millisecond)
	}
	// The vendor's wait_time_ms is an estimate that moves; a little headroom
	// past it avoids cutting off a turn that is about to be served.
	d := time.Duration(budget)*time.Millisecond + 30*time.Second
	t.mu.Lock()
	if until := time.Now().Add(d); until.After(t.queuedUntil) {
		t.queuedUntil = until
	}
	t.mu.Unlock()
}

// stallBudget is how long this turn may stay silent.
func (t *cliTurn) stallBudget() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.queuedUntil.IsZero() {
		return cliLineStall
	}
	left := time.Until(t.queuedUntil)
	if left < cliLineStall {
		return cliLineStall
	}
	return left
}

// newCLITurn wires one turn to its worker.  The reader goroutine starts here
// and always releases the worker's serial slot when it ends.
func newCLITurn(parent context.Context, p *cliProc) *cliTurn {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	t := &cliTurn{
		ctx:      ctx,
		cancel:   cancel,
		events:   make(chan core.Event, 64),
		proc:     p,
		finished: make(chan struct{}),
	}
	core.GoSafe("qoder cli turn", nil, t.read)
	return t
}

// Recv yields the next event, or io.EOF once at the clean end.
func (t *cliTurn) Recv() (core.Event, error) {
	select {
	case ev, ok := <-t.events:
		if !ok {
			return core.Event{}, io.EOF
		}
		return ev, nil
	case <-t.ctx.Done():
		return core.Event{}, t.ctx.Err()
	}
}

// Close cancels the turn.  It does NOT decide the worker's fate: a turn that
// settled cleanly leaves a perfectly good worker behind, and killing it here
// would make every request pay a cold start.  Discarding an abandoned worker is
// the lease's job (see cliLease.Close), which is the only caller that knows
// whether the turn reached the vendor's result line.
func (t *cliTurn) Close() error {
	t.once.Do(func() { t.cancel() })
	return nil
}

// emit delivers one event unless the turn was cancelled.
func (t *cliTurn) emit(ev core.Event) bool {
	select {
	case t.events <- ev:
		return true
	case <-t.ctx.Done():
		return false
	}
}

// read consumes worker lines until the turn settles, translating each into core
// events.  It owns nothing it does not close: the process outlives it.
func (t *cliTurn) read() {
	defer close(t.events)
	defer close(t.finished)
	defer t.proc.finish()

	var (
		usage     *core.Usage
		finish    string
		sawAny    bool
		lastError error
	)
	err := t.consume(func(ev core.Event) bool {
		sawAny = true
		if ev.Type == core.EventUsage && ev.Usage != nil {
			usage = ev.Usage
		}
		if ev.Type == core.EventDone {
			finish = ev.Finish
		}
		if ev.Type == core.EventError {
			lastError = ev.Err
		}
		return t.emit(ev)
	})

	switch {
	case t.ctx.Err() != nil:
		return
	case err != nil:
		t.proc.kill()
		t.emit(core.Event{Type: core.EventError, Err: err})
		return
	case lastError != nil:
		t.proc.kill()
		return
	case !sawAny:
		t.emit(core.Event{Type: core.EventError, Err: errors.New(
			"qoder: the Qoder CLI ended the turn without any output")})
		return
	}
	if usage != nil {
		t.emit(core.Event{Type: core.EventUsage, Usage: usage})
	}
	t.emit(core.Event{Type: core.EventDone, Finish: finish})
}

// cliLineStall is how long a worker may go without producing a single line
// before this turn gives up on it.  Cold start (including the CLI's own
// security-scan hook) is well inside this; a worker that is truly wedged is
// killed, the turn fails, and the next request starts a fresh one.
const cliLineStall = 2 * time.Minute

// consume walks the worker's lines for one turn, calling emit for each core
// event and returning the terminal error (nil on a clean settle).
func (t *cliTurn) consume(emit func(core.Event) bool) error {
	stopReason := ""
	for {
		p := t.proc
		p.mu.Lock()
		lines, werr := p.lines, p.werr
		p.mu.Unlock()
		if lines == nil {
			return errors.New("qoder: the Qoder CLI has no output pipe")
		}
		select {
		case <-t.ctx.Done():
			return t.ctx.Err()
		case <-time.After(t.stallBudget()):
			// The timer restarts on every line, so a slow-but-alive turn is
			// untouched; only a worker that has gone silent for the whole
			// window is abandoned here.
			return errors.New("qoder: the Qoder CLI stopped producing output")
		case raw, ok := <-lines:
			if !ok {
				if werr != nil && !errors.Is(werr, io.EOF) {
					return fmt.Errorf("qoder: the Qoder CLI stopped: %w", werr)
				}
				return nil
			}
			done, err := t.handleLine(raw, &stopReason, emit)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
	}
}

// handleLine translates one worker line.  It reports done=true once the turn
// has settled (the result line), with err carrying a vendor refusal.
func (t *cliTurn) handleLine(raw []byte, stopReason *string, emit func(core.Event) bool) (bool, error) {
	var line cliLine
	if err := json.Unmarshal(raw, &line); err != nil {
		// A line the bridge does not model is not fatal: newer SDK versions add
		// event kinds, and dropping one is better than breaking every turn.
		return false, nil
	}
	switch line.Type {
	case "system":
		// The vendor queues the free models when they are busy.  A turn that is
		// waiting must look like progress, not like a stalled worker: the stall
		// guard would otherwise kill a process that is doing exactly what the
		// vendor told it to do.
		if line.Subtype == "model_queue_status" {
			t.noteQueued(line.WaitTimeMS, line.QueueMaxWaitMS)
			if !emit(core.Event{Type: core.EventDelta, Reasoning: queueNotice(line)}) {
				return true, nil
			}
		}
		return false, nil
	case "stream_event":
		var ev cliEvent
		if err := json.Unmarshal(line.Event, &ev); err != nil {
			return false, nil
		}
		if ev.Delta.StopReason != "" {
			*stopReason = ev.Delta.StopReason
		}
		if ev.Type == "message_stop" {
			return false, nil
		}
		for _, coreEv := range eventsFromInner(ev) {
			if !emit(coreEv) {
				return true, nil
			}
		}
	case "result":
		t.markSettled()
		// A settled turn carries its own stop reason, and a single-shot turn
		// has no message_delta to have carried it earlier.
		if *stopReason == "" && line.StopReason != "" {
			*stopReason = line.StopReason
		}
		// The worker reports a signed-out account, and several other refusals,
		// as subtype "success" with is_error set.  Both spellings mean the turn
		// did not produce an answer, so both are surfaced to the gateway as a
		// classified failure rather than an empty completion.
		if line.IsError || (line.Subtype != "" && line.Subtype != "success") {
			return true, cliRefusalError(t.proc.account, refusalText(line))
		}
		if line.Usage != nil {
			if !emit(core.Event{Type: core.EventUsage, Usage: usageFromCLI(*line.Usage)}) {
				return true, nil
			}
		}
		if !emit(core.Event{Type: core.EventDone, Finish: finishFromCLI(*stopReason)}) {
			return true, nil
		}
		return true, nil
	}
	return false, nil
}

// queueNotice renders one queue report for the caller.  It goes on the
// reasoning channel so it never pollutes the answer text, and it keeps the
// caller informed during what can be a multi-minute wait.
func queueNotice(line cliLine) string {
	parts := make([]string, 0, 3)
	parts = append(parts, "排队中")
	if line.QueueCount > 0 {
		parts = append(parts, fmt.Sprintf("前面 %d 个请求", line.QueueCount))
	}
	if line.WaitTimeMS > 0 {
		parts = append(parts, fmt.Sprintf("预计 %s", (time.Duration(line.WaitTimeMS)*time.Millisecond).Round(time.Second)))
	}
	if !line.ServiceAvailable {
		parts = append(parts, "服务当前不可用")
	}
	return strings.Join(parts, "，") + "…"
}

// refusalText renders the vendor's own words.  They are kept verbatim: they are
// the only evidence of what went wrong.
func refusalText(line cliLine) string {
	msg := strings.TrimSpace(line.Result)
	if msg == "" && len(line.Errors) > 0 {
		msg = strings.TrimSpace(strings.Join(line.Errors, "; "))
	}
	if msg == "" {
		msg = "the Qoder CLI refused the turn"
	}
	return core.Redact(msg)
}

// cliUserLine renders one caller turn as the worker's stream-json input.
func cliUserLine(text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("qoder: empty prompt")
	}
	return json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	})
}

// promptFromRequest renders the caller's newest user turn.
//
// The worker is a chat agent that owns its own transcript, and the pool keys a
// process on the conversation, so only the newest user text is forwarded.
// History the caller re-sends is already in that process's session.
func promptFromRequest(req *core.ChatRequest) (string, error) {
	if req == nil {
		return "", errors.New("qoder: nil request")
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if !strings.EqualFold(strings.TrimSpace(m.Role), "user") {
			continue
		}
		text := strings.TrimSpace(m.Content)
		if text == "" && len(m.Parts) > 0 {
			var b strings.Builder
			for _, p := range m.Parts {
				if p.Type == "text" && p.Text != "" {
					if b.Len() > 0 {
						b.WriteString("\n")
					}
					b.WriteString(p.Text)
				}
			}
			text = strings.TrimSpace(b.String())
		}
		if text != "" {
			return text, nil
		}
	}
	return "", errors.New("qoder: no user turn to send")
}

// promptForTurn renders the prompt one turn sends.
//
// A worker that is already alive owns that conversation's transcript, so only
// the newest user turn goes over the wire.  A worker that is about to start --
// the first turn of a conversation, or the first turn after a sweep -- owns
// nothing, so the whole history is flattened into the prompt instead: sending
// just the newest turn there would silently drop the caller's context.
//
// The gateway derives a stickiness key from the message content itself when the
// caller sent no id, so "is this conversation sticky" is a property of the
// worker rather than of the request; that is why the choice is passed in.
func promptForTurn(req *core.ChatRequest, fresh bool) (string, error) {
	if !fresh {
		return promptFromRequest(req)
	}
	return flattenRequest(req)
}

// flattenRequest renders a whole stateless conversation as one prompt.
//
// Labels are kept because the model has to tell the turns apart, but a request
// that is a single user message is passed through bare, which is the shape the
// overwhelming majority of callers send.
func flattenRequest(req *core.ChatRequest) (string, error) {
	if req == nil || len(req.Messages) == 0 {
		return "", errors.New("qoder: no messages to send")
	}
	var b strings.Builder
	users := 0
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		text := strings.TrimSpace(m.Content)
		if text == "" && len(m.Parts) > 0 {
			var part strings.Builder
			for _, p := range m.Parts {
				if p.Type == "text" && p.Text != "" {
					if part.Len() > 0 {
						part.WriteString("\n")
					}
					part.WriteString(p.Text)
				}
			}
			text = strings.TrimSpace(part.String())
		}
		if text == "" {
			continue
		}
		switch role {
		case "system", "user", "assistant", "tool":
		default:
			continue
		}
		if role == "user" {
			users++
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(strings.ToUpper(role[:1]) + role[1:] + ": " + text)
	}
	switch {
	case b.Len() == 0:
		return "", errors.New("qoder: no user turn to send")
	case users == 1:
		// A single user turn needs no labels at all.
		if _, after, ok := strings.Cut(b.String(), "User: "); ok {
			return strings.TrimSpace(after), nil
		}
	}
	return b.String(), nil
}

// scanLines reads one JSON object per line.
func scanLines(r io.Reader, fn func([]byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if err := fn([]byte(line)); err != nil {
			return err
		}
	}
	return sc.Err()
}
