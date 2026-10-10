package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"testing"

	"client2api/internal/core"
)

// The usage page has to show the thinking level each call carried, so the
// gateway keeps whatever the caller sent.  Three spellings reach it in
// practice: codex++ hands the level over as a top-level reasoning_effort, a
// Responses-shaped client nests it under reasoning, and a client2api caller
// uses the options object the project documents for per-request knobs.
func TestReasoningEffortReadsTheSpellingsClientsSend(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"top level", `{"model":"m1","reasoning_effort":"xhigh","messages":[]}`, "xhigh"},
		{"responses object", `{"model":"m1","reasoning":{"effort":"high"},"messages":[]}`, "high"},
		{"client2api options", `{"model":"m1","client2api":{"reasoning_effort":"medium"},"messages":[]}`, "medium"},
		{"top level wins over options", `{"model":"m1","reasoning_effort":"low","client2api":{"reasoning_effort":"high"},"messages":[]}`, "low"},
		{"options win over the responses object", `{"model":"m1","reasoning":{"effort":"low"},"client2api":{"reasoning_effort":"high"},"messages":[]}`, "high"},
		{"trimmed and lowered", `{"model":"m1","reasoning_effort":"  XHigh  ","messages":[]}`, "xhigh"},
		{"absent", `{"model":"m1","messages":[]}`, ""},
		{"blank top level falls through", `{"model":"m1","reasoning_effort":"   ","client2api":{"reasoning_effort":"high"},"messages":[]}`, "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire chatRequest
			if err := json.Unmarshal([]byte(tc.body), &wire); err != nil {
				t.Fatalf("decode %s: %v", tc.body, err)
			}
			if got := reasoningEffortFor(&wire); got != tc.want {
				t.Errorf("reasoningEffortFor(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestChatRecordsTheReasoningEffortOnTheUsageRow(t *testing.T) {
	usage := NewUsageStore(8)
	srv := newTestServer(t, &testClient{name: "t", events: usageEvents(1, 1)}, NewStats(), usage)

	body := `{"model":"m1","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	got := usage.Snapshot()
	if len(got) != 1 {
		t.Fatalf("recorded %d usage records, want 1", len(got))
	}
	if got[0].ReasoningEffort != "xhigh" {
		t.Errorf("usage reasoning effort = %q, want xhigh", got[0].ReasoningEffort)
	}

	// A caller that names no level must stay blank: the row says what was sent,
	// it does not invent the module's configured default.
	plain := chat(t, srv, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if plain.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", plain.Code, plain.Body.String())
	}
	got = usage.Snapshot()
	if len(got) != 2 {
		t.Fatalf("recorded %d usage records, want 2", len(got))
	}
	if got[1].ReasoningEffort != "" {
		t.Errorf("usage reasoning effort = %q, want empty", got[1].ReasoningEffort)
	}
}

func TestChatReasoningEffortReachesUpstream(t *testing.T) {
	c := &testClient{name: "t", events: usageEvents(1, 1)}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(8))
	rec := chat(t, srv, `{"model":"m1","reasoning_effort":" XHIGH ","client2api":{"reasoning_effort":"low","other":true},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if c.seen.Options["reasoning_effort"] != "xhigh" || c.seen.Options["other"] != true {
		t.Errorf("upstream options = %v", c.seen.Options)
	}
}

// A failed candidate is its own recent-list row, and the thinking level belongs
// to the request rather than to the attempt, so every row of one request reads
// the same level.
func TestFailedCandidateAttemptsCarryTheReasoningEffort(t *testing.T) {
	alpha := &testClient{name: "alpha", chatErr: core.Fail("alpha", "a1", core.FailureUpstream, http.StatusBadGateway, errors.New("boom"))}
	beta := &testClient{name: "beta", events: usageEvents(1, 1)}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 1},
		"beta":  {Priority: 2},
	})
	usage := NewUsageStore(20)
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    usage,
	})

	body := `{"model":"m1","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var sawAttempt, sawSuccess bool
	for _, row := range usage.Snapshot() {
		switch {
		case row.Client == "alpha" && row.Failed && row.Attempt:
			sawAttempt = true
			if row.ReasoningEffort != "xhigh" {
				t.Errorf("failed attempt reasoning effort = %q, want xhigh", row.ReasoningEffort)
			}
		case row.Client == "beta" && !row.Failed:
			sawSuccess = true
			if row.ReasoningEffort != "xhigh" {
				t.Errorf("successful record reasoning effort = %q, want xhigh", row.ReasoningEffort)
			}
		}
	}
	if !sawAttempt || !sawSuccess {
		t.Fatalf("recent rows = %+v, want both the failed attempt and the success", usage.Snapshot())
	}
}
