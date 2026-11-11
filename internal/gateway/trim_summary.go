package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Folding the trimmed middle: a summary pinned into the window (issue #164).
//
// applyContextBudget keeps the leading system turns and the newest call-balanced
// tail and drops everything between them, oldest first.  For an agent caller
// (an opencode worker loop) that middle is the substance: files it read, patches
// it wrote, tool calls and their results.  The trim saves the request but hides
// a quality cost -- the model sees "task + the last move" and continues from
// amnesia.  The observed case was opencode/glm-5.3-flash-free trimming 2,100~
// 2,600 messages every single turn, 857K~1.05M tokens down to 168K.
//
// The fix follows Claude Code's auto-compact shape: fold the dropped middle
// into one handover note, and pin that note into the window ahead of the kept
// tail.  Two constraints shaped the implementation:
//
//   - The main request must never wait for a summarisation call.  A fold is a
//     full upstream round trip; doing it synchronously would turn every trim
//     into a multi-second stall.  So the fold runs in the background, and the
//     pinned summary appears on a LATER turn of the same conversation, once the
//     background fold has landed.
//   - The main request must never fail because the fold failed.  A fold error
//     costs nothing but a log line; the next turn retries the fold by itself.
//
// The fingerprint: system turns + the middle being folded, hashed verbatim.
// The agent loop resends its full history every turn, so for one conversation
// the middle is stable except for its oldest edge -- which means the fingerprint
// changes as the conversation grows, and with it the cache key.  That is the
// point: the summary describes exactly the middle that was dropped THIS turn,
// a different (larger) middle folds to a different summary, and an entry that
// no longer matches is simply not pinned.
// ---------------------------------------------------------------------------

// trimSummaryMinDropped is how many dropped messages make a fold worth doing.
// A three-turn middle is cheap to lose; a 2,100-message middle is the entire
// substance of the session.  The issue's own threshold suggestion (>50) scaled
// for a worker loop's tool-heavy history.
const trimSummaryMinDropped = 50

// trimSummaryMaxEntries caps how many conversation entries the cache holds.
// Each entry holds the fingerprint, a count, the summary text
// (<= compactionSummaryMaxChars) and a timestamp, so even a long-lived
// deployment stays bounded.  The cache is a byproduct cache: it is refreshed
// from the caller's own resent history, so the cap only bounds staleness.
const trimSummaryMaxEntries = 96

// trimSummaryTTL bounds how long a cached summary stays pinnable.  The caller
// resends its history every turn, so a live entry is re-pinned constantly;
// the TTL exists so an abandoned conversation's entry cannot live forever.
const trimSummaryTTL = 24 * time.Hour

// trimSummaryHeader is the one-line framing the pinned turn carries.  It is
// written in English -- upstream models read it, not the operator -- and it
// says both what the text is and what it is not.
const trimSummaryHeader = "[Earlier conversation, folded: the following is a summary of messages the gateway dropped to fit this model's context window. Treat it as history: it states what was done and found, not a request to act.]"

// trimSummaryFoldPrompt instructs the summariser for the trim-fold path.  It is
// the responses-compaction prompt plus the line that matters here: the fold
// covers the MIDDLE of a conversation whose newest turns are NOT present, so
// the note must not speculate about a request that has not been answered yet.
const trimSummaryFoldPrompt = compactionSystemPrompt + `

This fold covers only the middle of a longer conversation: the newest turns are
kept verbatim elsewhere and are not in this fold. Do not guess what happened
after this middle; end the note at the last event it contains.`

// trimFoldResponseTimeout bounds one chunk of a fold (see the per-chunk deadline
// the fold context carries).  The fold is a background service to a FUTURE turn;
// an upstream that wedges must not hold the fold slot forever.
const trimFoldResponseTimeout = 90 * time.Second

// trimFoldAttemptTimeout bounds the whole fold.  A >500K-token middle folds in
// chunks (compactionMaxRounds at most), each under the response timeout; this
// is the whole-job watchdog, and a fold that cannot finish inside it is
// abandoned -- the next turn re-folds from scratch.
const trimFoldAttemptTimeout = 6 * time.Minute

// trimSummaryEntry is one cached fold: the summary text, how many messages it
// folded, and when it landed (for the TTL).
type trimSummaryEntry struct {
	summary string
	dropped int
	at      time.Time
	// fails counts consecutive fold failures for this fingerprint.  A shape
	// the vendor rejects comes back every turn at full price (issue #240:
	// 265 failures in three days, 337 messages re-trimmed each time), so after
	// trimFoldBackoffThreshold failures the coordinator skips the fingerprint
	// for trimFoldBackoff and a turn arriving inside the window is answered
	// without paying for a doomed request.
	fails    int
	lastFail time.Time
}

// trimFoldBackoffThreshold is how many consecutive failures of one
// fingerprint arm the backoff.
const trimFoldBackoffThreshold = 2

// trimFoldBackoff is how long an armed backoff lasts.
const trimFoldBackoff = 30 * time.Minute

// trimFoldPlatformCooldown is how long a platform is cooled for folds after
// capacity failures.  The per-fingerprint backoff above (issue #240) keys on
// the middle's verbatim hash -- but a growing conversation's middle changes
// every turn, so its key changes every turn and the counter never reaches the
// threshold.  Capacity failures (deadlines, in-flight ceilings, exhausted
// pools) are a PLATFORM's verdict, not a conversation's: the same wedged
// upstream times out whatever middle it is handed (issue #256: raccoon paid a
// 6-minute timeout pair roughly every hour for eleven hours, loomy one 90s
// stall per hour, because each turn presented a fresh key).  Cooling the
// platform for folds bounds that cost while leaving the main request path
// untouched.
const trimFoldPlatformCooldown = 30 * time.Minute

// trimFoldPlatformCooldownThreshold is how many consecutive platform-verdict
// failures arm the platform cooldown.
const trimFoldPlatformCooldownThreshold = 2

// trimSummaryStore caches folds keyed by the middle's fingerprint.  A nil store
// is usable and simply never pins, which keeps the embedder paths honest.
type trimSummaryStore struct {
	mu      sync.Mutex
	entries map[string]trimSummaryEntry
	// platforms holds per-platform fold-cooldown bookkeeping: consecutive
	// capacity-verdict failures and when the last one happened (issue #256).
	// Keyed by the client name -- a fold rides the platform's own routing, so
	// the platform is the unit the vendor's health decides.
	platforms map[string]foldPlatformState
}

// foldPlatformState is one platform's consecutive capacity-failure count for
// folds.
type foldPlatformState struct {
	fails    int
	lastFail time.Time
}

func newTrimSummaryStore() *trimSummaryStore {
	return &trimSummaryStore{entries: map[string]trimSummaryEntry{}, platforms: map[string]foldPlatformState{}}
}

// get returns the summary cached for this key, if fresh.
func (s *trimSummaryStore) get(key string) (string, bool) {
	if s == nil || key == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return "", false
	}
	if time.Since(e.at) > trimSummaryTTL {
		// An abandoned conversation's handover note must not pin into a later
		// session that happens to land on the same fingerprint.
		delete(s.entries, key)
		return "", false
	}
	return e.summary, true
}

// recordFoldFailure bumps the fingerprint's consecutive-failure counter and
// timestamps it (issue #240).
func (s *trimSummaryStore) recordFoldFailure(key string) int {
	if s == nil || key == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		e = trimSummaryEntry{at: time.Now()}
	}
	e.fails++
	e.lastFail = time.Now()
	s.entries[key] = e
	return e.fails
}

// foldBackedOff reports whether this fingerprint is inside a backoff window:
// it has failed trimFoldBackoffThreshold times in a row and the last failure is
// recent.  A fingerprint is a shape, not a conversation, so the backoff stops
// the same doomed request from being paid every turn (issue #240).
func (s *trimSummaryStore) foldBackedOff(key string) (bool, int) {
	if s == nil || key == "" {
		return false, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || e.fails < trimFoldBackoffThreshold {
		return false, e.fails
	}
	if time.Since(e.lastFail) > trimFoldBackoff {
		// The window has lapsed: one fresh attempt is allowed to re-probe.
		e.fails = 0
		s.entries[key] = e
		return false, 0
	}
	return true, e.fails
}

// recordFoldPlatformFailure bumps the platform's consecutive capacity-failure
// counter and timestamps it (issue #256).  Capacity verdicts -- deadlines,
// in-flight ceilings, exhausted pools -- blame the platform, not the middle:
// the same wedged upstream times out whatever conversation it is handed, and a
// growing conversation changes its key every turn, so the fingerprint-level
// counter never reaches the threshold.  The platform cooldown is what bounds
// that cost.
func (s *trimSummaryStore) recordFoldPlatformFailure(platform string) int {
	if s == nil || platform == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.platforms[platform]
	p.fails++
	p.lastFail = time.Now()
	s.platforms[platform] = p
	return p.fails
}

// recordFoldPlatformSuccess clears the platform's consecutive-failure counter:
// a fold that just served through this platform is evidence the vendor is
// healthy again (issue #256).
func (s *trimSummaryStore) recordFoldPlatformSuccess(platform string) {
	if s == nil || platform == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.platforms, platform)
}

// foldPlatformCooled reports whether this platform is inside a fold cooldown:
// it has produced trimFoldPlatformCooldownThreshold capacity failures in a row
// and the last one is recent.  Inside the window the offer gate skips the fold
// for every conversation on the platform -- the wedge is not conversation
// specific (issue #256).
func (s *trimSummaryStore) foldPlatformCooled(platform string) (bool, int) {
	if s == nil || platform == "" {
		return false, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.platforms[platform]
	if !ok || p.fails < trimFoldPlatformCooldownThreshold {
		return false, p.fails
	}
	if time.Since(p.lastFail) > trimFoldPlatformCooldown {
		// The cooldown has lapsed: one fresh attempt is allowed to re-probe.
		delete(s.platforms, platform)
		return false, 0
	}
	return true, p.fails
}

// trimSummariesBackedOff exposes the store's backoff state to the offer gate.
func (c *trimFoldCoordinator) trimSummariesBackedOff(key string) (bool, int) {
	if c == nil {
		return false, 0
	}
	return c.store.foldBackedOff(key)
}

// trimSummariesPlatformCooled exposes the store's platform-cooldown state to
// the offer gate (issue #256).
func (c *trimFoldCoordinator) trimSummariesPlatformCooled(platform string) (bool, int) {
	if c == nil {
		return false, 0
	}
	return c.store.foldPlatformCooled(platform)
}

// foldPlatformCooldownVerdict reports whether err is a PLATFORM capacity
// verdict rather than a conversation-shape refusal (issue #256).  Capacity
// failures cool the whole platform's folds; vendor content/shape refusals keep
// the per-fingerprint backoff, because those depend on what was folded and the
// shape snapshot is the evidence chain (issue #240).
func foldPlatformCooldownVerdict(err error) bool {
	if err == nil {
		return false
	}
	// An exhausted pool or a saturated in-flight ceiling: the platform's own
	// verdict, whatever middle it was handed.
	if errors.Is(err, core.ErrPlatformExhausted) || errors.Is(err, core.ErrBusy) ||
		errors.Is(err, core.ErrNotConfigured) {
		return true
	}
	// A caller that walked away blames nobody; do not cool anything.
	if errors.Is(err, context.Canceled) {
		return false
	}
	// Deadline/stall: the platform did not answer in time.  The fold wraps its
	// own watchdog timeout, so the deadline here may be the fold's rather than
	// the vendor's -- either way the conversation did not cause it.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// A classified failure keeps the fingerprint-level backoff unless the kind
	// itself is a platform-health verdict: transport and 5xx are the vendor's
	// capacity, not the middle's content.
	if f, ok := core.AsFailure(err); ok {
		return f.Kind == core.FailureUpstream
	}
	// An unclassified transport fault (normalizeUnclassifiedFailure's input
	// vocabulary) is also the platform's health.
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range transportFaultMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// put records a fold, evicting the oldest entry when the cache is full.
func (s *trimSummaryStore) put(key, summary string, dropped int) {
	if s == nil || key == "" || summary == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= trimSummaryMaxEntries {
		// Evict by oldest timestamp.  n is tiny (<= 96), so a linear scan
		// beats importing a heap for a byproduct cache.
		var oldest string
		var oldestAt time.Time
		for k, e := range s.entries {
			if oldest == "" || e.at.Before(oldestAt) {
				oldest, oldestAt = k, e.at
			}
		}
		delete(s.entries, oldest)
	}
	s.entries[key] = trimSummaryEntry{summary: summary, dropped: dropped, at: time.Now()}
}

// ---------------------------------------------------------------------------
// Fingerprinting the middle
// ---------------------------------------------------------------------------

// trimMiddleFingerprint hashes exactly the messages a trim dropped: the system
// turns are included verbatim (part of what the summary must carry) and the
// middle follows.  Every field the estimator counts rides in the hash, so two
// middles the estimator reads differently can never share a fingerprint.
func trimMiddleFingerprint(system, middle []core.Message) string {
	h := sha256.New()
	for _, m := range system {
		writeMessageFingerprint(h, 's', m)
	}
	for _, m := range middle {
		writeMessageFingerprint(h, 'm', m)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeMessageFingerprint writes one message's identity into the hash.  tag
// separates the pinned system turns ('s') from the dropped middle ('m') so the
// same text in a different position cannot alias.
func writeMessageFingerprint(h io.Writer, tag byte, m core.Message) {
	io.WriteString(h, "m\n")
	h.Write([]byte{tag, 0})
	io.WriteString(h, m.Role)
	h.Write([]byte{0})
	io.WriteString(h, m.Content)
	h.Write([]byte{0})
	io.WriteString(h, m.Reasoning)
	h.Write([]byte{0})
	for _, p := range m.Parts {
		h.Write([]byte{'p', 0})
		io.WriteString(h, p.Type)
		h.Write([]byte{0})
		io.WriteString(h, p.Text)
		h.Write([]byte{0})
	}
	for _, tc := range m.ToolCalls {
		h.Write([]byte{'t', 0})
		io.WriteString(h, tc.Name)
		h.Write([]byte{0})
		io.WriteString(h, tc.Arguments)
		h.Write([]byte{0})
	}
}

// trimSummaryCacheKey is the full cache key: fingerprint + dropped count, so a
// content collision between differently sized middles cannot cross-contaminate.
func trimSummaryCacheKey(fp string, dropped int) string {
	return fp + ":" + strconv.Itoa(dropped)
}

// ---------------------------------------------------------------------------
// The background fold
// ---------------------------------------------------------------------------

// trimFoldJob is one pending fold: the messages to fold and where the result
// goes.
type trimFoldJob struct {
	key     string
	system  []core.Message
	middle  []core.Message
	dropped int
	client  core.Client
	model   string
}

// foldSummariser folds one middle into a summary.  The server installs the real
// one (which routes through the normal module path); tests install stubs.
type foldSummariser func(ctx context.Context, job *trimFoldJob) (string, error)

// trimFoldCoordinator runs folds in the background, one at a time.  A request
// that arrives while a fold is running replaces the queued job: the agent loop
// resends its full history every turn, so the newest middle is always the most
// valuable one to fold.  The goroutine is started lazily on the first offer.
type trimFoldCoordinator struct {
	once sync.Once

	summarise foldSummariser
	// done / failed are the server's callbacks into its own store and logger.
	done   func(job *trimFoldJob, summary string)
	failed func(job *trimFoldJob, err error)
	// logf reports the offer-side backoff skips; nil keeps the skip silent
	// (tests).
	logf func(format string, args ...any)
	// store is the summary cache, which also keeps the per-fingerprint
	// failure bookkeeping the offer gate reads.
	store *trimSummaryStore

	mu   sync.Mutex
	next *trimFoldJob
	wake chan struct{}
}

func newTrimFoldCoordinator(summarise foldSummariser, done func(*trimFoldJob, string), failed func(*trimFoldJob, error), store *trimSummaryStore, logf func(string, ...any)) *trimFoldCoordinator {
	return &trimFoldCoordinator{
		summarise: summarise,
		logf:      logf,
		store:     store,
		done:      done,
		failed:    failed,
		wake:      make(chan struct{}, 1),
	}
}

// offer hands a middle to the coordinator.  It never blocks the request path:
// if a fold is running, the job replaces whatever was queued (a queued middle
// is by definition from an older turn than this one); the caller's next turn
// offers its own middle again, so nothing is lost -- only deferred.
func (c *trimFoldCoordinator) offer(job *trimFoldJob) {
	c.once.Do(func() { go c.work() })
	c.mu.Lock()
	c.next = job
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// work is the single fold worker: take the pending job (if any), fold it, and
// park on the wake channel until the next offer.
func (c *trimFoldCoordinator) work() {
	for {
		<-c.wake
		for {
			c.mu.Lock()
			job := c.next
			c.next = nil
			c.mu.Unlock()
			if job == nil {
				break
			}
			c.fold(job)
		}
	}
}

// fold runs one summarisation chain and stores the result.  Every failure mode
// is the failed callback and nothing else: the fold exists to serve FUTURE
// turns, and a failed fold just means those turns pin nothing -- exactly the
// pre-fix behaviour.
func (c *trimFoldCoordinator) fold(job *trimFoldJob) {
	if c.summarise == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), trimFoldAttemptTimeout)
	defer cancel()
	summary, err := c.summarise(ctx, job)
	if err != nil {
		if c.failed != nil {
			c.failed(job, err)
		}
		return
	}
	if c.done != nil {
		c.done(job, summary)
	}
}

// ---------------------------------------------------------------------------
// The real summariser: the compaction path's chunked fold, with the
// trim-specific instruction
// ---------------------------------------------------------------------------

// foldSummarise reuses the responses-compaction fold's chunking
// (splitForSummarisation keeps tool round trips whole, mergeOldest bounds the
// rounds) with the fold prompt and the per-chunk deadline.  It routes through
// openStream, so account selection, rotation and accounting behave exactly the
// way a user-visible call does.
func (s *server) foldSummarise(ctx context.Context, job *trimFoldJob) (string, error) {
	if job == nil || job.client == nil {
		return "", errors.New("fold: no platform to summarise with")
	}
	var parts []string
	for _, chunk := range splitForSummarisation(job.middle, compactionInputBudget) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		callCtx, cancel := context.WithTimeout(ctx, trimFoldResponseTimeout)
		text, _, err := s.summariseTurns(callCtx, job.client, job.model, job.system, chunk)
		cancel()
		if err != nil {
			return "", err
		}
		if text = strings.TrimSpace(text); text != "" {
			parts = append(parts, text)
		}
	}
	summary := strings.Join(parts, "\n\n")
	if strings.TrimSpace(summary) == "" {
		return "", errors.New("fold: the summariser returned nothing")
	}
	if len(summary) > compactionSummaryMaxChars {
		summary = summary[:compactionSummaryMaxChars]
	}
	return summary, nil
}

// summariseTurns is compaction.go's summariseMessages with two changes the fold
// needs: the system prompt is the fold prompt (not the compaction one), and the
// system turns of the original conversation lead the fold call so the
// summariser knows the task.  It goes through the normal routing path
// (openStream), so rotation and accounting apply here too.
// foldShapeSnapshot renders one compact line per message: role, sizes, and the
// shape markers a strict validator could reject on -- WITHOUT the content
// itself, so the transcript stays free of credentials and private text while
// still answering "which message did the validator choke on" (issue #240
// reopen: the second blind fix needs evidence, not guesses).
func foldShapeSnapshot(msgs []core.Message) string {
	var b strings.Builder
	for i, m := range msgs {
		role := strings.TrimSpace(m.Role)
		b.WriteString(fmt.Sprintf("[%d]%s", i, role))
		b.WriteString(fmt.Sprintf("(len=%d", len(m.Content)))
		if m.Reasoning != "" {
			b.WriteString(",reason")
		}
		if len(m.ToolCalls) > 0 {
			ids := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				ids = append(ids, strings.TrimSpace(tc.ID))
			}
			b.WriteString(",calls=" + strings.Join(ids, "|"))
		}
		if id := strings.TrimSpace(m.ToolCallID); id != "" {
			b.WriteString(",for=" + id)
		}
		if len(m.Parts) > 0 {
			b.WriteString(fmt.Sprintf(",parts=%d", len(m.Parts)))
		}
		b.WriteString(")")
		if i < len(msgs)-1 {
			b.WriteString(" ")
		}
	}
	return b.String()
}

func (s *server) summariseTurns(ctx context.Context, client core.Client, model string, system, msgs []core.Message) (string, *core.Usage, error) {
	body := core.RepairToolPairing(core.RepairToolArguments(append([]core.Message(nil), msgs...)))
	// The fold prompt and the conversation's own system turns are MERGED into
	// one system message (issue #240: the shape snapshots show every fold
	// request opens with TWO adjacent system messages, and 81001 keeps
	// refusing even fully-paired shapes -- the dual-system head is the one
	// feature direct chats never carry).  Body repair follows the merge.
	merged := make([]string, 0, len(system)+1)
	merged = append(merged, trimSummaryFoldPrompt)
	for _, m := range system {
		if t := strings.TrimSpace(m.Content); t != "" {
			merged = append(merged, t)
		}
	}
	// 单条消息字节上限（issue #240 第二轮实验）：63K 的单条 user / 38K 的单条
	// tool 是被拒请求里仅剩的可疑特征——收紧到 16K 字符，超出截断并标注。若
	// 81001 消失 → 体量实锤；仍拒 → 内容审查实锤。
	const foldSingleMessageMaxChars = 16 * 1024
	for i := range body {
		if len(body[i].Content) > foldSingleMessageMaxChars {
			body[i].Content = body[i].Content[:foldSingleMessageMaxChars] +
				"\n\n[... 已截断：原长度 " + fmt.Sprintf("%d", len(body[i].Content)) + " 字符 ...]"
		}
	}
	reqMsgs := make([]core.Message, 0, len(body)+1)
	reqMsgs = append(reqMsgs, core.Message{Role: "system", Content: strings.Join(merged, "\n\n")})
	reqMsgs = append(reqMsgs, body...)
	// 修形后置到合并之后（issue #240）：body 单独修过形，但拼接边界仍可能让
	// 孤立 tool 结果站在请求前段。合并后的 repair 覆盖全部边界；首条仍为
	// tool 时再垫一条说明（校验器普遍要求首条非 tool）。
	reqMsgs = core.RepairToolPairing(core.RepairToolArguments(reqMsgs))
	if len(reqMsgs) > 0 && strings.EqualFold(strings.TrimSpace(reqMsgs[0].Role), "tool") {
		reqMsgs = append([]core.Message{{Role: "user", Content: "(以下是被折叠会话前段的工具调用结果，供摘要参考)"}}, reqMsgs...)
	}
	// 末尾垫一条 user（issue #240 第三轮实验，opencode 同族确认）：纯工具会话
	// 的 middle 拼出的请求是 system → assistant(tool_calls) → tool → … →
	// assistant，全程无 user——officeace 与 opencode 两家厂商都拒这种形状。
	// 垫一条 user 说明任务，同时覆盖「system 后无 user」与「全程无 user」
	// 两条嫌疑；直连对话永远有 user，垫上后形状与直连对齐。
	if len(reqMsgs) > 0 && !strings.EqualFold(strings.TrimSpace(reqMsgs[len(reqMsgs)-1].Role), "user") {
		reqMsgs = append(reqMsgs, core.Message{Role: "user", Content: "（以上是被折叠会话的历史记录，请按 system 指令总结）"})
	}
	req := &core.ChatRequest{
		Model:    model,
		Messages: reqMsgs,
		Stream:   true,
	}
	stream, err := s.openStream(ctx, client, req)
	if err != nil {
		// The vendor refused the request itself: the shape snapshot is the
		// evidence a second fix needs (issue #240 reopen).
		s.opts.Logger.Printf("context: fold request refused before streaming; shape snapshot: %s", foldShapeSnapshot(reqMsgs))
		return "", nil, err
	}
	defer stream.Close()

	var (
		text strings.Builder
		use  *core.Usage
	)
	for {
		ev, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return "", nil, rerr
		}
		switch ev.Type {
		case core.EventDelta:
			if ev.Delta != "" {
				text.WriteString(ev.Delta)
				if text.Len() > compactionSummaryMaxChars {
					return text.String()[:compactionSummaryMaxChars], use, nil
				}
			}
		case core.EventUsage:
			if ev.Usage != nil {
				use = ev.Usage
			}
		case core.EventDone:
			return text.String(), use, nil
		case core.EventError:
			if ev.Err != nil {
				s.opts.Logger.Printf("context: fold stream refused by vendor; shape snapshot: %s", foldShapeSnapshot(reqMsgs))
				return "", nil, ev.Err
			}
		}
	}
	return text.String(), use, nil
}
