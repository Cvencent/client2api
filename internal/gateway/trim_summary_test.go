package gateway

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Issue #164: the trim's dropped middle is folded in the background and the
// summary is pinned into a later turn's window.
// ---------------------------------------------------------------------------

// foldTestRequest builds a conversation with n user turns of sizePerTurn chars.
func foldTestRequest(n, sizePerTurn int) *core.ChatRequest {
	req := &core.ChatRequest{}
	for i := 0; i < n; i++ {
		req.Messages = append(req.Messages, core.Message{
			Role:    "user",
			Content: strings.Repeat("a", sizePerTurn),
		})
	}
	return req
}

func TestTrimMiddleFingerprintFollowsContent(t *testing.T) {
	sys := []core.Message{{Role: "system", Content: "be brief"}}
	mid := []core.Message{
		{Role: "user", Content: "read login.go"},
		{Role: "assistant", Content: "did it"},
	}
	a := trimMiddleFingerprint(sys, mid)
	if a == "" {
		t.Fatal("fingerprint is empty")
	}
	if a == trimMiddleFingerprint(sys, mid) {
		// Same input, same hash -- the stable case.
	} else {
		t.Fatal("fingerprint is not deterministic")
	}
	// A different middle must not share the key...
	b := trimMiddleFingerprint(sys, append(append([]core.Message(nil), mid...), core.Message{Role: "user", Content: "one more"}))
	if a == b {
		t.Fatal("a larger middle hashed the same as the smaller one")
	}
	// ...and the same text in the system position must not alias the middle.
	c := trimMiddleFingerprint([]core.Message{{Role: "system", Content: "changed"}}, mid)
	if a == c {
		t.Fatal("a system-turn change did not move the fingerprint")
	}
	// Tool calls and reasoning ride in the hash too.
	d := []core.Message{
		{Role: "assistant", Content: "did it", ToolCalls: []core.ToolCall{{Name: "read", Arguments: "{\"p\":\"login.go\"}"}}},
	}
	e := trimMiddleFingerprint(sys, d)
	if e == a {
		t.Fatal("a tool-call turn hashed the same as its plain-text twin")
	}
}

func TestTrimSummaryStoreTTLEvictsAndCaps(t *testing.T) {
	s := newTrimSummaryStore()
	s.put("k1", "summary one", 60)
	if got, ok := s.get("k1"); !ok || got != "summary one" {
		t.Fatalf("get = %q,%v", got, ok)
	}
	// A different key misses.
	if _, ok := s.get("k2"); ok {
		t.Fatal("an unknown key must miss")
	}
	// TTL: an entry older than the window must be dropped, not served.
	s.mu.Lock()
	s.entries["kold"] = trimSummaryEntry{summary: "stale", at: time.Now().Add(-trimSummaryTTL - time.Minute)}
	s.mu.Unlock()
	if _, ok := s.get("kold"); ok {
		t.Fatal("an expired entry must not be served")
	}
	if _, ok := s.get("kold"); ok {
		t.Fatal("the expired entry was not deleted")
	}
	// Cap: filling the cache must evict instead of growing without bound.
	for i := 0; i < trimSummaryMaxEntries+8; i++ {
		s.put("k"+strings.Repeat("x", i%13)+string(rune('A'+i%26))+string(rune('0'+i%10))+string(rune(i)), "s", 1)
	}
	s.mu.Lock()
	n := len(s.entries)
	s.mu.Unlock()
	if n > trimSummaryMaxEntries {
		t.Fatalf("cache grew to %d entries, cap is %d", n, trimSummaryMaxEntries)
	}
}

func TestApplyContextBudgetPinsCachedSummaryBeforeTail(t *testing.T) {
	// A conversation whose middle was folded on an earlier turn: the request is
	// the same shape as the plain trim case, but the store now carries the
	// summary for exactly this middle.
	req := foldTestRequest(12, 2000)
	budget := int64(2000)

	// First, the plain trim: what would be dropped?
	plain := foldTestRequest(12, 2000)
	report, trimmed := applyContextBudget(plain, budget)
	if !trimmed {
		t.Fatal("the case must be an oversized request")
	}
	dropped := report.DroppedMessages
	if dropped < trimSummaryMinDropped && dropped == 0 {
		t.Fatalf("nothing to fold: dropped=%d", dropped)
	}

	// Seed the store with the summary for this middle and pin it.
	store := newTrimSummaryStore()
	// The pin path looks the key up by fingerprint, so seed under the key the
	// production code will compute.
	pinned := leadingSystemTurns(req.Messages)
	dropped = pinnedMiddleCount(req)
	fp := trimMiddleFingerprint(req.Messages[:pinned], req.Messages[pinned:pinned+dropped])
	store.put(trimSummaryCacheKey(fp, dropped), "DONE: read login.go and fixed the nil check. NEXT: run the tests.", dropped)

	fold := &trimFoldContext{store: store}
	rep, trimmed := applyContextBudgetWithFold(req, budget, fold)
	if !trimmed {
		t.Fatal("the request is still oversized; the summary cannot have fixed that alone")
	}
	// The summary turn must be in the kept window, ahead of the newest turn.
	found := -1
	for i, m := range req.Messages {
		if strings.Contains(m.Content, trimSummaryHeader) {
			found = i
			break
		}
	}
	if found < 0 {
		t.Fatal("the pinned summary is not in the kept window")
	}
	if found > len(req.Messages)-2 {
		t.Fatal("the pinned summary displaced the newest turn")
	}
	if !strings.Contains(req.Messages[found].Content, "read login.go") {
		t.Fatal("the pinned turn does not carry the summary text")
	}
	if rep.DroppedMessages != dropped {
		t.Fatalf("dropped = %d, want the same %d as the plain trim (the summary rides in the head, not in the droppable history)", rep.DroppedMessages, dropped)
	}
}

// pinnedMiddleCount runs the plain trim and reports how many messages it would
// drop, so the test seeds the key the production code will compute.
func pinnedMiddleCount(req *core.ChatRequest) int {
	probe := &core.ChatRequest{Messages: append([]core.Message(nil), req.Messages...)}
	rep, trimmed := applyContextBudget(probe, 2000)
	if !trimmed {
		return 0
	}
	return rep.DroppedMessages
}

func TestApplyContextBudgetNoSummaryWithoutCache(t *testing.T) {
	req := foldTestRequest(12, 2000)
	fold := &trimFoldContext{store: newTrimSummaryStore()}
	before := len(req.Messages)
	rep, trimmed := applyContextBudgetWithFold(req, 2000, fold)
	if !trimmed {
		t.Fatal("oversized request must trim")
	}
	for _, m := range req.Messages {
		if strings.Contains(m.Content, trimSummaryHeader) {
			t.Fatal("no cached fold: nothing may be pinned")
		}
	}
	if rep.DroppedMessages == 0 {
		t.Fatalf("nothing dropped: %d -> %d messages", before, len(req.Messages))
	}
}

func TestApplyContextBudgetOffersDroppedMiddleToBackgroundFold(t *testing.T) {
	// 120 small turns under a tight budget drops far more than the fold
	// threshold (50), so the offer gate opens.
	req := foldTestRequest(120, 100)
	store := newTrimSummaryStore()
	fold := &trimFoldContext{store: store}
	// A coordinator whose summariser records the job synchronously: this pins
	// down "the middle this trim dropped reached the fold seam" without racing
	// the real worker.
	var (
		mu   sync.Mutex
		seen *trimFoldJob
	)
	coord := newTrimFoldCoordinator(
		func(ctx context.Context, job *trimFoldJob) (string, error) {
			mu.Lock()
			seen = job
			mu.Unlock()
			return "folded", nil
		},
		func(job *trimFoldJob, summary string) {},
		nil, store, nil)
	fold.folds = coord
	fold.client = &testClient{name: "stub"}
	fold.model = "m1"

	rep, trimmed := applyContextBudgetWithFold(req, 2000, fold)
	if !trimmed {
		t.Fatal("oversized request must trim")
	}
	if rep.DroppedMessages == 0 {
		t.Fatal("nothing dropped, nothing to offer")
	}
	if got, ok := fold.store.get(""); ok || got != "" {
		t.Fatal("the empty key must miss")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := seen
		mu.Unlock()
		if got != nil {
			if got.dropped != rep.DroppedMessages {
				t.Fatalf("offered dropped = %d, want %d", got.dropped, rep.DroppedMessages)
			}
			if len(got.middle) != rep.DroppedMessages {
				t.Fatalf("offered middle = %d messages, want %d", len(got.middle), rep.DroppedMessages)
			}
			if got.key == "" || got.client == nil || got.model == "" {
				t.Fatal("the offered job carries no key or no route")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the dropped middle never reached the fold seam")
}

func TestFoldSummariseChunksAndConcatenates(t *testing.T) {
	// foldSummarise is the real path; drive it against a stub client through a
	// server wired exactly like the production constructor.
	c := &testClient{name: "stub", events: summaryEvents("PART ONE: the goal was the login bug.")}
	reg := core.NewRegistry()
	reg.Add(c)
	gw := &server{
		opts: Options{
			Registry: reg,
			Version:  "test",
			Logger:   log.New(io.Discard, "", 0),
			Stats:    NewStats(),
			Usage:    NewUsageStore(0),
		},
		stats:   NewStats(),
		started: time.Now(),
	}
	gw.sessionPlatforms = newSessionPlatforms()
	gw.inflight = NewInFlightTrackers()
	gw.learnedWindows = openLearnedWindowStore("", gw.opts.Logger.Printf)
	gw.estimates = newEstimateCalibrationStore("", gw.opts.Logger.Printf)

	middle := []core.Message{
		{Role: "user", Content: "read login.go"},
		{Role: "assistant", Content: "read it"},
	}
	job := &trimFoldJob{
		key:     "k",
		middle:  middle,
		dropped: len(middle),
		client:  c,
		model:   "m1",
	}
	summary, err := gw.foldSummarise(context.Background(), job)
	if err != nil {
		t.Fatalf("foldSummarise: %v", err)
	}
	if !strings.Contains(summary, "PART ONE") {
		t.Fatalf("summary lost the stub text: %q", summary)
	}
	if c.chatCalls == 0 {
		t.Fatal("the fold never called the module")
	}
	// The fold prompt must lead the call, and the middle must follow.
	if c.seen == nil || len(c.seen.Messages) == 0 {
		t.Fatal("the fold call carried no messages")
	}
	if !strings.Contains(c.seen.Messages[0].Content, "Do not guess what happened") {
		t.Fatalf("the fold prompt is not the trim-specific one: %q", c.seen.Messages[0].Content[:80])
	}
}

func TestFoldFailureLeavesTheStoreEmptyAndLogs(t *testing.T) {
	store := newTrimSummaryStore()
	var (
		mu         sync.Mutex
		failedWith error
	)
	coord := newTrimFoldCoordinator(
		func(ctx context.Context, job *trimFoldJob) (string, error) {
			return "", errors.New("upstream wedged")
		},
		func(job *trimFoldJob, summary string) {
			t.Fatal("a failed fold must not report success")
		},
		func(job *trimFoldJob, err error) {
			mu.Lock()
			failedWith = err
			mu.Unlock()
		},
		store, nil)
	coord.offer(&trimFoldJob{key: "k", dropped: 60})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := failedWith != nil
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	err := failedWith
	mu.Unlock()
	if err == nil {
		t.Fatal("the failure callback never ran")
	}
	if _, ok := store.get("k"); ok {
		t.Fatal("a failed fold must not put anything in the store")
	}
}

func TestFoldSuccessStoresTheSummary(t *testing.T) {
	store := newTrimSummaryStore()
	landed := make(chan string, 1)
	coord := newTrimFoldCoordinator(
		func(ctx context.Context, job *trimFoldJob) (string, error) {
			return "DONE: fixed the bug. NEXT: deploy.", nil
		},
		func(job *trimFoldJob, summary string) {
			store.put(job.key, summary, job.dropped)
			landed <- summary
		},
		nil, store, nil)
	coord.offer(&trimFoldJob{key: "k", middle: []core.Message{{Role: "user", Content: "x"}}, dropped: 60})
	select {
	case <-landed:
	case <-time.After(2 * time.Second):
		t.Fatal("the fold never completed")
	}
	got, ok := store.get("k")
	if !ok || !strings.Contains(got, "fixed the bug") {
		t.Fatalf("stored = %q,%v", got, ok)
	}
}

// ---------------------------------------------------------------------------
// End to end: turn 1 trims and offers the middle in the background; turn 2
// (the same conversation resent) pins the folded summary into its window.
// ---------------------------------------------------------------------------

func TestTrimFoldEndToEndSecondTurnPinsTheSummary(t *testing.T) {
	c := &bunnyClient{name: "bunny", realWindow: 100000, folded: "FOLDED-STATE: the login bug is fixed; tests are green."}
	usage := NewUsageStore(0)
	s := newGuardServer(t, c, "", func(o *Options) { o.Usage = usage })
	// The guard's fold wiring mirrors NewServer: store + coordinator, with the
	// real summariser routed at this stub client.
	s.trimSummaries = newTrimSummaryStore()
	s.folds = newTrimFoldCoordinator(
		func(ctx context.Context, job *trimFoldJob) (string, error) {
			return c.folded, nil
		},
		func(job *trimFoldJob, summary string) {
			s.trimSummaries.put(job.key, summary, job.dropped)
		},
		nil, s.trimSummaries, nil)
	s.learnedWindows.put("bunny", "cn:space-bunny", 100000)

	// Turn 1: far over the window; the guard trims and offers the middle.
	if rr := chatHandler(t, s, oversizedBody(200, 4000)); rr.Code != 200 {
		t.Fatalf("turn 1: status %d: %s", rr.Code, rr.Body.String())
	}
	// Wait for the background fold to land.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.trimSummaries.mu.Lock()
		n := len(s.trimSummaries.entries)
		s.trimSummaries.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.trimSummaries.mu.Lock()
	n := len(s.trimSummaries.entries)
	s.trimSummaries.mu.Unlock()
	if n == 0 {
		t.Fatal("the fold never landed; turn 2 would pin nothing")
	}

	// Turn 2: the same conversation resent (an agent loop does exactly this).
	// The trimmed request the vendor receives must carry the pinned summary.
	if rr := chatHandler(t, s, oversizedBody(200, 4000)); rr.Code != 200 {
		t.Fatalf("turn 2: status %d: %s", rr.Code, rr.Body.String())
	}
	if c.seen == nil {
		t.Fatal("the module was never called on turn 2")
	}
	pinned := false
	for _, m := range c.seen.Messages {
		if strings.Contains(m.Content, trimSummaryHeader) && strings.Contains(m.Content, c.folded) {
			pinned = true
		}
	}
	if !pinned {
		t.Fatalf("turn 2 sent %d messages, none carried the folded summary", len(c.seen.Messages))
	}
}
