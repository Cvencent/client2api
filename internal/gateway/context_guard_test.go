package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Reading the limit out of a refusal
// ---------------------------------------------------------------------------

func TestParseLimitFromMessage(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want int64
	}{
		{"workbuddy code 11115", `{"code":11115,"msg":"prompt is too long: 100001 tokens > 100000 maximum","requestId":"b15aeebef2a4f830ddc58866a5288569"}`, 100000},
		{"workbuddy larger window", `prompt is too long: 1060027 tokens > 1048576 maximum`, 1048576},
		{"comma separated", "prompt is too long: 100,001 tokens > 100,000 maximum", 100000},
		{"underscore separated", "prompt is too long: 100001 tokens > 1048576 maximum", 1048576},
		{"maximum phrased after", "maximum context length is 200000 tokens", 200000},
		{"exceeds phrased", "request exceeds the model's 128000 token limit", 128000},
		{"no number at all", "prompt is too long", 0},
		{"unrelated refusal", "account credits exhausted at upstream", 0},
		{"implausibly small", "prompt is too long: 500 tokens > 400 maximum", 0},
		{"empty", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLimitFromMessage(tc.msg); got != tc.want {
				t.Fatalf("parseLimitFromMessage(%q) = %d, want %d", tc.msg, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The estimator
// ---------------------------------------------------------------------------

func TestEstimateTextTokensIsSanelyOrdered(t *testing.T) {
	short := estimateTextTokens("hello")
	long := estimateTextTokens(strings.Repeat("hello world ", 100))
	if short <= 0 {
		t.Fatalf("a non-empty string must cost at least one token, got %d", short)
	}
	if long <= short*100 {
		t.Fatalf("100x the text must cost noticeably more: %d vs %d", long, short)
	}
}

func TestEstimateTextTokensCountsCJKWhole(t *testing.T) {
	// Ideographs are roughly one token each in the tokenizers these vendors use.
	cjk := strings.Repeat("\u4f60\u597d", 100) // 200 ideographs
	if got := estimateTextTokens(cjk); got < 150 {
		t.Fatalf("CJK estimate = %d, want roughly one token per ideograph", got)
	}
}

func TestEstimateRequestTokensCountsToolsAndHistory(t *testing.T) {
	bare := &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: "hi"}}}
	withHistory := &core.ChatRequest{Messages: []core.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: strings.Repeat("x", 4000)},
	}}
	if estimateRequestTokens(withHistory) <= estimateRequestTokens(bare) {
		t.Fatal("history must add to the estimate")
	}
	withTools := &core.ChatRequest{
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Tools: []core.Tool{{
			Name:        "read_file",
			Description: strings.Repeat("d", 2000),
			Parameters:  []byte(`{"type":"object"}`),
		}},
	}
	if estimateRequestTokens(withTools) <= estimateRequestTokens(bare) {
		t.Fatal("tool declarations must add to the estimate")
	}
}

// ---------------------------------------------------------------------------
// Trimming
// ---------------------------------------------------------------------------

// buildRequest returns a conversation of n turns, each roughly sizePerTurn
// characters of ASCII.
func buildRequest(n, sizePerTurn int) *core.ChatRequest {
	req := &core.ChatRequest{}
	for i := 0; i < n; i++ {
		req.Messages = append(req.Messages, core.Message{
			Role:    "user",
			Content: strings.Repeat("a", sizePerTurn),
		})
	}
	return req
}

func TestApplyContextBudgetLeavesFittingRequestAlone(t *testing.T) {
	req := buildRequest(3, 100)
	before := estimateRequestTokens(req)
	report, trimmed := applyContextBudget(req, int64(before*2))
	if trimmed {
		t.Fatalf("a request that fits must not be trimmed, dropped %d", report.DroppedMessages)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages changed: %d", len(req.Messages))
	}
}

func TestApplyContextBudgetNeverRunsWithoutABudget(t *testing.T) {
	// A budget of 0 means "window unknown": the request must pass through
	// untouched, because rewriting a request that would have succeeded is worse
	// than one honest 400.
	req := buildRequest(20, 2000)
	original := estimateRequestTokens(req)
	_, trimmed := applyContextBudget(req, 0)
	if trimmed {
		t.Fatal("an unknown window must never trigger a trim")
	}
	if estimateRequestTokens(req) != original {
		t.Fatal("the request was modified despite an unknown window")
	}
}

func TestApplyContextBudgetTrimsOldestAndKeepsTheFinalTurn(t *testing.T) {
	req := buildRequest(10, 2000)
	final := req.Messages[len(req.Messages)-1]
	if _, trimmed := applyContextBudget(req, 5000); !trimmed {
		t.Fatal("an oversized request must be trimmed")
	}
	if len(req.Messages) >= 10 {
		t.Fatalf("nothing was dropped: %d messages", len(req.Messages))
	}
	if got := req.Messages[len(req.Messages)-1]; got.Content != final.Content {
		t.Fatal("the newest turn must survive the trim: it is the question being asked")
	}
	if int64(estimateRequestTokens(req)) > 5000 {
		t.Fatalf("trimmed request is still over budget: %d", estimateRequestTokens(req))
	}
}

func TestApplyContextBudgetReportsImpossibleRatherThanCuttingTheQuestion(t *testing.T) {
	// One turn far larger than the budget: cutting into it would answer a
	// different question, so the guard refuses instead.
	req := buildRequest(1, 100000)
	report, trimmed := applyContextBudget(req, 1000)
	if trimmed {
		t.Fatal("an impossible request must not be trimmed")
	}
	if !report.Impossible {
		t.Fatal("the report must mark the request impossible")
	}
	if len(req.Messages) != 1 {
		t.Fatal("a refused request must be left exactly as the caller sent it")
	}
}

func TestApplyContextBudgetTrimsToTheFinalTurnInsteadOfRefusing(t *testing.T) {
	// A request whose only fitting suffix is the final turn itself must be
	// trimmed to that turn.  The guard used to read the loop landing index
	// as "nothing fits" and refuse a request it could have served -- which
	// is the shape a conversation hits right after a large tool result (a
	// screenshot) comes back.
	req := buildRequest(2, 4000)
	final := req.Messages[len(req.Messages)-1]
	report, trimmed := applyContextBudget(req, 1500)
	if !trimmed {
		t.Fatal("a request that fits with only the final turn must be trimmed, not refused")
	}
	if report.Impossible {
		t.Fatal("a trimmable request must not be reported impossible")
	}
	if len(req.Messages) != 1 {
		t.Fatalf("only the final turn should survive, got %d messages", len(req.Messages))
	}
	if req.Messages[0].Content != final.Content {
		t.Fatal("the surviving turn must be the newest one")
	}
	if int64(estimateRequestTokens(req)) > 1500 {
		t.Fatalf("trimmed request is still over budget: %d", estimateRequestTokens(req))
	}
}

func TestApplyContextBudgetKeepsTheSystemPrompt(t *testing.T) {
	// The system prompt is not stale history: it is the task. Dropping it would
	// change what the model is being asked to do, so it is pinned alongside the
	// final turn and only the turns between them are droppable.
	req := &core.ChatRequest{Messages: []core.Message{
		{Role: "system", Content: "you are a careful assistant"},
		{Role: "user", Content: strings.Repeat("a", 2000)},
		{Role: "assistant", Content: strings.Repeat("b", 2000)},
		{Role: "user", Content: strings.Repeat("c", 2000)},
		{Role: "assistant", Content: strings.Repeat("d", 2000)},
		{Role: "user", Content: "the actual question"},
	}}
	if _, trimmed := applyContextBudget(req, 900); !trimmed {
		t.Fatal("expected a trim")
	}
	if req.Messages[0].Role != "system" {
		t.Fatalf("the system prompt was dropped, conversation now starts with %q", req.Messages[0].Role)
	}
	if got := req.Messages[len(req.Messages)-1].Content; got != "the actual question" {
		t.Fatalf("the newest turn was dropped, conversation now ends with %q", got)
	}
}

// ---------------------------------------------------------------------------
// Learning
// ---------------------------------------------------------------------------

func TestLearnedWindowStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "learned_context_windows.json")
	s := openLearnedWindowStore(path, nil)
	if got := s.get("workbuddy", "cn:space-bunny"); got != 0 {
		t.Fatalf("a fresh store must know nothing, got %d", got)
	}
	if !s.put("workbuddy", "cn:space-bunny", 100000) {
		t.Fatal("put must report a change")
	}
	if s.put("workbuddy", "cn:space-bunny", 100000) {
		t.Fatal("putting the same value again must report no change")
	}

	reopened := openLearnedWindowStore(path, nil)
	if got := reopened.get("workbuddy", "cn:space-bunny"); got != 100000 {
		t.Fatalf("the learned limit must survive a restart, got %d", got)
	}
}

func TestLearnedWindowStoreSurvivesCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "learned_context_windows.json")
	writeFile(t, path, "{not json")
	var warned []string
	s := openLearnedWindowStore(path, func(f string, a ...any) { warned = append(warned, f) })
	if got := s.get("workbuddy", "cn:space-bunny"); got != 0 {
		t.Fatalf("a corrupt file must read as empty, got %d", got)
	}
	if len(warned) == 0 {
		t.Fatal("a corrupt file must be reported")
	}
	// And it must still be usable.
	if !s.put("workbuddy", "cn:space-bunny", 100000) {
		t.Fatal("the store must remain writable after a corrupt read")
	}
}

// writeFile is a test helper for files the production code only ever reads.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
