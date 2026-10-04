package workbuddy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/prompt"
)

// wireBodyWithSystem is a minimal chat body shaped like the one buildWireBody
// produces: one system turn carrying the client's own template text, one user
// turn that must survive every prompt strategy.
func wireBodyWithSystem(system, user string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4-flash",
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	return b
}

func messagesOf(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal rewritten body %s: %v", body, err)
	}
	return doc.Messages
}

func systemTurns(t *testing.T, body []byte) []string {
	t.Helper()
	var out []string
	for _, m := range messagesOf(t, body) {
		if m["role"] == "system" {
			s, _ := m["content"].(string)
			out = append(out, s)
		}
	}
	return out
}

func userTurns(t *testing.T, body []byte) []string {
	t.Helper()
	var out []string
	for _, m := range messagesOf(t, body) {
		if m["role"] == "user" {
			s, _ := m["content"].(string)
			out = append(out, s)
		}
	}
	return out
}

// A client that never picks a strategy must still behave like the reference:
// passthrough.  The zero value of the field is not one of the three strategies,
// so this is the test that catches forgetting to initialise it.
func TestANewClientStartsInPassthrough(t *testing.T) {
	c, _ := newTestClient(t, nil)
	if mode, text := c.promptState(); mode != promptModePassthrough || text != "" {
		t.Fatalf("prompt state = %q/%q, want passthrough with no text", mode, text)
	}
}

func TestPassthroughLeavesTheBodyByteForByte(t *testing.T) {
	c, _ := newTestClient(t, nil)
	body := wireBodyWithSystem("CLIENT SYSTEM", "hi")
	if got := c.applyPrompt(body, false); string(got) != string(body) {
		t.Fatalf("passthrough rewrote the body to %s", got)
	}
}

func TestCustomModeReplacesTheClientsSystemTurn(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.setPromptState(promptModeCustom, "OPERATOR PROMPT")

	got := c.applyPrompt(wireBodyWithSystem("CLIENT SYSTEM", "hi"), false)
	if sys := systemTurns(t, got); len(sys) != 1 || sys[0] != "OPERATOR PROMPT" {
		t.Fatalf("system turns = %q, want exactly the operator's prompt", sys)
	}
	// The operator replaced the system prompt, not the conversation.
	if user := userTurns(t, got); len(user) != 1 || user[0] != "hi" {
		t.Fatalf("user turns = %q, want the conversation intact", user)
	}
}

func TestAppendModeKeepsTheClientsTurnAndAddsOurs(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.setPromptState(promptModeAppend, "OPERATOR PROMPT")

	got := c.applyPrompt(wireBodyWithSystem("CLIENT SYSTEM", "hi"), false)
	sys := systemTurns(t, got)
	if len(sys) != 2 || sys[0] != "CLIENT SYSTEM" || sys[1] != "OPERATOR PROMPT" {
		t.Fatalf("system turns = %q, want the client's turn then ours", sys)
	}
	if user := userTurns(t, got); len(user) != 1 || user[0] != "hi" {
		t.Fatalf("user turns = %q, want the conversation intact", user)
	}
}

// The reference's degraded window is sticky until the next 00:00 CST, so every
// request in it is rewritten, not only the one that opened it.  Append
// deliberately collapses to replace inside the window: retrying with the
// client's own system text is a deterministic way to be refused again.
func TestTheDegradedWindowRewritesEveryRequestInIt(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.deps.Guard = core.NewGuard(nil)
	c.setPromptState(promptModeAppend, "OPERATOR PROMPT")
	c.deps.Guard.ReportContentBlock()

	got := c.applyPrompt(wireBodyWithSystem("CLIENT SYSTEM", "hi"), false)
	if sys := systemTurns(t, got); len(sys) != 1 || sys[0] != prompt.Degraded {
		t.Fatalf("system turns = %q, want the minimal prompt only", sys)
	}
}

// Custom mode is the operator's explicit choice, so neither the window nor the
// retry hook is allowed to overwrite it with the built-in fallback.
func TestCustomModeIgnoresTheDegradedWindow(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.deps.Guard = core.NewGuard(nil)
	c.setPromptState(promptModeCustom, "OPERATOR PROMPT")
	c.deps.Guard.ReportContentBlock()

	got := c.applyPrompt(wireBodyWithSystem("CLIENT SYSTEM", "hi"), false)
	if sys := systemTurns(t, got); len(sys) != 1 || sys[0] != "OPERATOR PROMPT" {
		t.Fatalf("system turns = %q, want the operator's own prompt", sys)
	}
}

func TestDegradeMarksTheRequestSoTheRetryIsRewritten(t *testing.T) {
	c, _ := newTestClient(t, nil)
	req := &core.ChatRequest{Model: "deepseek-v4-flash"}

	if !c.Degrade(req) {
		t.Fatal("Degrade refused a passthrough request, so the gateway would never retry it")
	}
	if !degradeRequested(req) {
		t.Fatal("Degrade reported true without marking the request")
	}
	// The marker is what carries the decision across the gateway's retry, so
	// the retry has to actually come out with the minimal prompt.
	got := c.applyPrompt(wireBodyWithSystem("CLIENT SYSTEM", "hi"), degradeRequested(req))
	if sys := systemTurns(t, got); len(sys) != 1 || sys[0] != prompt.Degraded {
		t.Fatalf("system turns on the retry = %q, want the minimal prompt only", sys)
	}
}

func TestDegradeRefusesACustomPrompt(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.setPromptState(promptModeCustom, "OPERATOR PROMPT")
	req := &core.ChatRequest{}

	if c.Degrade(req) {
		t.Fatal("Degrade spent its one retry replacing a prompt the operator chose on purpose")
	}
	if degradeRequested(req) {
		t.Fatal("Degrade marked a request it refused")
	}
}

func TestDegradeRefusesWhenTheWindowIsAlreadyOpen(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.deps.Guard = core.NewGuard(nil)
	c.setPromptState(promptModeAppend, "OPERATOR PROMPT")
	c.deps.Guard.ReportContentBlock()

	if c.Degrade(&core.ChatRequest{}) {
		t.Fatal("Degrade offered a retry for a request that already used the minimal prompt")
	}
}

func TestApplyLiveLoadsACustomPromptFromDisk(t *testing.T) {
	c, dir := newTestClient(t, nil)
	path := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(path, []byte("OPERATOR PROMPT\n"), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	c.ApplyLive(core.LiveSettings{PromptMode: "custom", PromptFile: path})

	mode, text := c.promptState()
	if mode != promptModeCustom || text != "OPERATOR PROMPT\n" {
		t.Fatalf("prompt state = %q/%q, want the file's text verbatim", mode, text)
	}
	got := c.applyPrompt(wireBodyWithSystem("CLIENT SYSTEM", "hi"), false)
	if sys := systemTurns(t, got); len(sys) != 1 || sys[0] != "OPERATOR PROMPT\n" {
		t.Fatalf("system turns = %q, want the text that was loaded", sys)
	}
}

// A replacement prompt that cannot be read must not take the module down: the
// operator gets a log line and passthrough, which still works.
func TestApplyLiveFallsBackWhenThePromptCannotBeRead(t *testing.T) {
	c, dir := newTestClient(t, nil)
	c.setPromptState(promptModeCustom, "OPERATOR PROMPT")
	c.ApplyLive(core.LiveSettings{PromptMode: "custom", PromptFile: filepath.Join(dir, "absent.md")})

	if mode, text := c.promptState(); mode != promptModePassthrough || text != "" {
		t.Fatalf("prompt state after a failed load = %q/%q, want passthrough", mode, text)
	}
}

func TestApplyLiveFallsBackWhenThePromptIsBlank(t *testing.T) {
	c, dir := newTestClient(t, nil)
	path := filepath.Join(dir, "blank.md")
	if err := os.WriteFile(path, []byte("  \n\t\n"), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	c.ApplyLive(core.LiveSettings{PromptMode: "append", PromptFile: path})

	if mode, _ := c.promptState(); mode != promptModePassthrough {
		t.Fatalf("mode = %q, want passthrough for a blank prompt", mode)
	}
}

func TestApplyLiveIgnoresAnEmptySettingsValue(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.setPromptState(promptModeAppend, "OPERATOR PROMPT")
	c.ApplyLive(core.LiveSettings{})

	if mode, text := c.promptState(); mode != promptModeAppend || text != "OPERATOR PROMPT" {
		t.Fatalf("an empty reload cleared the strategy to %q/%q", mode, text)
	}
}

func TestApplyLiveKeepsTheCurrentModeWhenTheValueIsUnknown(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.setPromptState(promptModeAppend, "OPERATOR PROMPT")
	c.ApplyLive(core.LiveSettings{PromptMode: "rewrite"})

	if mode, _ := c.promptState(); mode != promptModeAppend {
		t.Fatalf("mode = %q, want the previous append", mode)
	}
}

func TestApplyLiveTogglesTheBodyScrubber(t *testing.T) {
	c, _ := newTestClient(t, nil)
	if !c.up.sanitizeOn() {
		t.Fatal("the scrubber is off by default; the reference defaults it on")
	}
	off := false
	c.ApplyLive(core.LiveSettings{SanitizeFingerprints: &off})
	if c.up.sanitizeOn() {
		t.Fatal("the scrubber is still on after a live reload turned it off")
	}
	on := true
	c.ApplyLive(core.LiveSettings{SanitizeFingerprints: &on})
	if !c.up.sanitizeOn() {
		t.Fatal("the scrubber is still off after a live reload turned it on")
	}
	// A reload that says nothing about the scrubber must not clear it.
	c.ApplyLive(core.LiveSettings{PromptMode: "passthrough"})
	if !c.up.sanitizeOn() {
		t.Fatal("a partial reload cleared a value the operator did not touch")
	}
}

// session_sticky.* used to be parsed, defaulted and logged with nothing reading
// it.  Three things have to hold now: the window moves, a partial reload leaves
// it alone, and the prompt branch's early return does not swallow it.
func TestApplyLiveMovesTheConversationStickinessWindow(t *testing.T) {
	c, _ := newTestClient(t, nil)
	if got := c.affinity.TTL(); got != core.DefaultAffinityTTL {
		t.Fatalf("window = %s by default, want %s", got, core.DefaultAffinityTTL)
	}

	c.ApplyLive(core.LiveSettings{AffinityTTL: 2 * time.Hour, AffinityGCInterval: 10 * time.Minute})
	if got := c.affinity.TTL(); got != 2*time.Hour {
		t.Fatalf("window = %s after a reload, want 2h", got)
	}
	if got := c.affinity.GCInterval(); got != 10*time.Minute {
		t.Fatalf("sweep = %s after a reload, want 10m", got)
	}

	// A reload that says nothing about stickiness must leave it where it is.
	off := false
	c.ApplyLive(core.LiveSettings{SanitizeFingerprints: &off})
	if got := c.affinity.TTL(); got != 2*time.Hour {
		t.Fatalf("window = %s after a partial reload, want 2h", got)
	}

	// And the push still reaches it when the prompt mode is empty, which is the
	// branch that returns early.
	c.ApplyLive(core.LiveSettings{PromptMode: "", AffinityTTL: 45 * time.Minute})
	if got := c.affinity.TTL(); got != 45*time.Minute {
		t.Fatalf("window = %s with an empty prompt mode, want 45m", got)
	}
}

// The prompt stage has to stay out of the per-attempt pipeline: Append is not
// idempotent, so a rotation retry would stack a second copy of the operator's
// prompt on the first.  This test pins the call site to Client.Chat.
func TestThePerAttemptPipelineDoesNotRunThePromptStage(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.setPromptState(promptModeAppend, "OPERATOR PROMPT")

	body := wireBodyWithSystem("CLIENT SYSTEM", "hi")
	prepared := c.up.prepareBody(body, "cn", "uid-test-0001", "conv-1")
	for _, s := range systemTurns(t, prepared) {
		if s == "OPERATOR PROMPT" {
			t.Fatal("the per-attempt pipeline applied the prompt stage; a retry would append it twice")
		}
	}
}
