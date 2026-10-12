package gateway

import (
	"context"
	"fmt"
	"testing"

	"client2api/internal/core"
)

// The production scenario, measured from the running instance's usage ledger:
// workbuddy refused cn:space-bunny with "100000 maximum" at 10:32, and the very
// same model was then observed serving a 224986 token prompt successfully.  A
// refusal number is therefore not always the model's real window, and learning it
// verbatim would shrink the model to less than half of what it demonstrably does.
const (
	observedServedPrompt = 224986
	vendorRefusalLimit   = 100000
)

// seedSuccess records a successful request for the model, the way the gateway's
// own usage ledger would have after the model answered.
func seedSuccess(usage *UsageStore, model string, promptTokens int) {
	usage.Record(UsageRecord{
		Client:       "workbuddy",
		Model:        model,
		PromptTokens: promptTokens,
	})
}

func TestLearnFloorRejectsARefusalFarBelowWhatWasServed(t *testing.T) {
	usage := NewUsageStore(0)
	seedSuccess(usage, "cn:space-bunny", observedServedPrompt)

	c := &exhaustedClient{
		name:    "workbuddy",
		window:  1000000,
		chatErr: workbuddyTooLong(),
	}
	s := newGuardServer(t, c, t.TempDir()+"/learned.json", func(o *Options) { o.Usage = usage })

	limit := s.learnContextWindowFromFailure(context.Background(), c, "cn:space-bunny", workbuddyTooLong())

	// 224986 * 4/5 = 179988.8 -> 179988, which is above the refusal's 100000.
	want := int64(observedServedPrompt) * learnWindowFloorNumerator / learnWindowFloorDenominator
	if limit != want {
		t.Fatalf("learned limit = %d, want the floor %d", limit, want)
	}
	if limit <= vendorRefusalLimit {
		t.Fatalf("the floor must lift the limit above the refusal, got %d", limit)
	}
}

// The floor must also stop the *recorded* value from being the small one, since
// that is what later requests are budgeted against.
func TestLearnFloorIsWhatGetsPersisted(t *testing.T) {
	usage := NewUsageStore(0)
	seedSuccess(usage, "cn:space-bunny", observedServedPrompt)

	c := &exhaustedClient{name: "workbuddy", window: 1000000, chatErr: workbuddyTooLong()}
	path := t.TempDir() + "/learned.json"
	s := newGuardServer(t, c, path, func(o *Options) { o.Usage = usage })
	s.learnContextWindowFromFailure(context.Background(), c, "cn:space-bunny", workbuddyTooLong())

	reopened := openLearnedWindowStore(path, nil)
	got := reopened.get("workbuddy", "cn:space-bunny")
	want := int64(observedServedPrompt) * learnWindowFloorNumerator / learnWindowFloorDenominator
	if got != want {
		t.Fatalf("persisted limit = %d, want %d", got, want)
	}
	// And the effective window the guard judges against must be the floored one,
	// never the raw refusal.
	if eff := s.effectiveContextWindow(context.Background(), c, "cn:space-bunny"); eff != want {
		t.Fatalf("effective window = %d, want %d", eff, want)
	}
}

// With no evidence of what the model can serve, the refusal is believed as-is.
// The floor must not invent a number out of nothing.
func TestLearnFloorStaysOutOfTheWayWithoutEvidence(t *testing.T) {
	c := &exhaustedClient{name: "workbuddy", window: 1000000, chatErr: workbuddyTooLong()}
	s := newGuardServer(t, c, t.TempDir()+"/learned.json")

	if got := s.learnContextWindowFromFailure(context.Background(), c, "cn:space-bunny", workbuddyTooLong()); got != 100000 {
		t.Fatalf("without evidence the refusal must be believed, got %d", got)
	}
}

// A refusal that is already above the floor is left alone: the floor only ever
// corrects an over-shrunk number.
func TestLearnFloorLeavesAReasonableRefusalAlone(t *testing.T) {
	usage := NewUsageStore(0)
	seedSuccess(usage, "cn:space-bunny", observedServedPrompt)

	// 200000 is above 80% of 224986, so it stands.
	inner := core.Fail("workbuddy", "acct", core.FailureContextWindow, 400,
		fmt.Errorf("prompt is too long: 200001 tokens > 200000 maximum"))
	c := &exhaustedClient{name: "workbuddy", window: 1000000, chatErr: inner}
	s := newGuardServer(t, c, t.TempDir()+"/learned.json", func(o *Options) { o.Usage = usage })

	if got := s.learnContextWindowFromFailure(context.Background(), c, "cn:space-bunny", inner); got != 200000 {
		t.Fatalf("a refusal above the floor must be kept verbatim, got %d", got)
	}
}

// Failed rows are not evidence: a refusal never reached the model, so its
// prompt_tokens must not raise the floor.
func TestLearnFloorIgnoresFailedRows(t *testing.T) {
	usage := NewUsageStore(0)
	usage.Record(UsageRecord{
		Client:       "workbuddy",
		Model:        "cn:space-bunny",
		PromptTokens: 900000,
		Failed:       true,
	})
	c := &exhaustedClient{name: "workbuddy", window: 1000000, chatErr: workbuddyTooLong()}
	s := newGuardServer(t, c, t.TempDir()+"/learned.json", func(o *Options) { o.Usage = usage })

	if got := s.learnContextWindowFromFailure(context.Background(), c, "cn:space-bunny", workbuddyTooLong()); got != 100000 {
		t.Fatalf("a failed row must not raise the floor, got %d", got)
	}
}

// Another model's successes must not raise this model's floor.
func TestLearnFloorIsPerModel(t *testing.T) {
	usage := NewUsageStore(0)
	seedSuccess(usage, "cn:some-other-model", 900000)

	c := &exhaustedClient{name: "workbuddy", window: 1000000, chatErr: workbuddyTooLong()}
	s := newGuardServer(t, c, t.TempDir()+"/learned.json", func(o *Options) { o.Usage = usage })

	if got := s.learnContextWindowFromFailure(context.Background(), c, "cn:space-bunny", workbuddyTooLong()); got != 100000 {
		t.Fatalf("another model's history must not raise this model's floor, got %d", got)
	}
}
