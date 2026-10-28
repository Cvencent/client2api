package gateway

import (
	"io"
	"log"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/livecfg"
)

// A model-scoped refusal ("this account type cannot use this model", code
// 11158 from workbuddy) must fail over to the next platform in a bare-model /
// model-group chain: the account is healthy, the model exists elsewhere.
func TestModelRefusedKindFailsOver(t *testing.T) {
	err := core.Fail("workbuddy", "acct", core.FailureModelRefused, http.StatusBadRequest, errString("model [gpt-6-astra] is not allowed for user type"))
	if !modelRefusal(err) {
		t.Fatalf("modelRefusal = false, want true for a FailureModelRefused 400")
	}
	if core.Retryable(core.FailureModelRefused) {
		t.Fatal("FailureModelRefused must not be retryable on the same platform")
	}
}

// A relay that cannot serve tools for a model answers the openai-compat
// module's 400 "当前模型或上游不支持工具调用", now classified as a
// model-scoped refusal.  A request that arrived through a model group must
// move to the sibling platform instead of the caller seeing the 400; an
// explicitly named platform must keep receiving the honest refusal.
func TestModelRefusedTool400FailsOverWithinGroup(t *testing.T) {
	toolRefusal := core.Fail("tokenrhythm", "key-1", core.FailureModelRefused, http.StatusBadRequest,
		errString("openai-compat: chat (HTTP 400): 当前模型或上游不支持工具调用"))
	tokenrhythm := &testClient{
		name:      "tokenrhythm",
		catalogue: []core.Model{{ID: "deepseek-v4.1-flash-1017"}},
		chatErr:   toolRefusal,
	}
	raccoon := &testClient{
		name:      "raccoon",
		catalogue: []core.Model{{ID: "sn-deepseek-v4-1-flash-1017"}},
		servedBy:  "raccoon-1",
		events:    usageEvents(1, 1),
	}

	reg := core.NewRegistry()
	reg.Add(tokenrhythm)
	reg.Add(raccoon)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"tokenrhythm": {Priority: 1},
		"raccoon":     {Priority: 2},
	})
	reg.SetModelGroups(map[string]core.ModelGroup{
		"deepseek-v4.1-flash": {Members: []core.ModelGroupMember{
			{Client: "tokenrhythm", Model: "deepseek-v4.1-flash-1017"},
			{Client: "raccoon", Model: "sn-deepseek-v4-1-flash-1017"},
		}},
	})

	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
		Live:     livecfg.New(livecfg.Snapshot{MaxRotate: 1, RotateBackoffBase: time.Nanosecond}),
	})

	// The group request fails over: the caller gets the sibling's answer, and
	// the refusing platform saw exactly one attempt.
	rec := chat(t, srv, `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("group status = %d, body %s", rec.Code, rec.Body.String())
	}
	if tokenrhythm.seen == nil {
		t.Fatal("the refusing platform never received the group request")
	}
	if raccoon.seen == nil {
		t.Fatal("the tool-refusing platform was not skipped to its group sibling")
	}

	// Naming the refusing platform explicitly is answered honestly: the 400
	// reaches the caller because the caller chose that platform.
	servedBefore := raccoon.chatCalls
	rec = chat(t, srv, `{"model":"tokenrhythm/deepseek-v4.1-flash-1017","messages":[{"role":"user","content":"again"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("explicit platform status = %d, body %s", rec.Code, rec.Body.String())
	}
	if raccoon.chatCalls != servedBefore {
		t.Fatalf("raccoon served %d more calls, want 0 for an explicit platform request", raccoon.chatCalls-servedBefore)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
