package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// windowClient publishes a per-model context window, records the last request
// it received, and can be made to refuse with an unclassified model-not-found
// answer so the router moves on to the next platform.
type windowClient struct {
	name   string
	window int64
	seen   *core.ChatRequest
	calls  int
	fail   bool
}

func (c *windowClient) Name() string { return c.name }

func (c *windowClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "m1", OwnedBy: c.name, Extra: map[string]any{
		"context_length": c.window,
	}}}, nil
}

func (c *windowClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.calls++
	c.seen = req
	if c.fail {
		return nil, core.Fail(c.name, "", core.FailureModelRefused, http.StatusNotFound, errors.New("model not found"))
	}
	return &fakeStream{events: []core.Event{{Type: core.EventDone, Finish: "stop"}}}, nil
}

func (c *windowClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

func candidateTrimServer(t *testing.T, usage *UsageStore, clients ...core.Client) *http.Server {
	t.Helper()
	reg := core.NewRegistry()
	for _, c := range clients {
		reg.Add(c)
	}
	return NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    usage,
	})
}

func candidateTrimBody(turns, sizePerTurn int) string {
	msgs := make([]map[string]string, 0, turns)
	for i := 0; i < turns; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]string{"role": role, "content": strings.Repeat("a", sizePerTurn)})
	}
	b, _ := json.Marshal(map[string]any{
		"model":    "m1",
		"messages": msgs,
		"stream":   false,
	})
	return string(b)
}

func TestFailoverToSmallerWindowTrimsBeforeSending(t *testing.T) {
	large := &windowClient{name: "large", window: 1000000, fail: true}
	small := &windowClient{name: "small", window: 20000}
	usage := NewUsageStore(10)
	srv := candidateTrimServer(t, usage, large, small)

	rr := chat(t, srv, candidateTrimBody(40, 3000))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if small.seen == nil {
		t.Fatal("the smaller-window candidate was never called")
	}
	if got := int64(estimateRequestTokens(small.seen)); got > 20000 {
		t.Fatalf("the failover request was not trimmed to the smaller window: %d tokens", got)
	}
}

func TestSmallerFirstCandidateDoesNotShrinkHistoryForALargerLaterCandidate(t *testing.T) {
	small := &windowClient{name: "small", window: 20000, fail: true}
	large := &windowClient{name: "large", window: 1000000}
	srv := candidateTrimServer(t, NewUsageStore(10), small, large)

	rr := chat(t, srv, candidateTrimBody(40, 3000))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if large.seen == nil {
		t.Fatal("the larger-window candidate was never called")
	}
	if got := len(large.seen.Messages); got != 40 {
		t.Fatalf("the larger candidate received %d messages, want the full 40", got)
	}
}

func TestTrimmingForOneCandidateDoesNotLeakIntoTheNext(t *testing.T) {
	// The first candidate is small enough to trim, then fails; the second is
	// large enough to serve the whole history and must receive all of it.
	small := &windowClient{name: "small", window: 20000, fail: true}
	large := &windowClient{name: "large", window: 1000000}
	srv := candidateTrimServer(t, NewUsageStore(10), small, large)

	rr := chat(t, srv, candidateTrimBody(30, 2000))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := len(large.seen.Messages); got != 30 {
		t.Fatalf("the larger candidate received %d messages, want the untrimmed 30", got)
	}
}

func TestFinalSuccessRecordsTheTrimAndServingWindow(t *testing.T) {
	large := &windowClient{name: "large", window: 1000000, fail: true}
	small := &windowClient{name: "small", window: 20000}
	usage := NewUsageStore(10)
	srv := candidateTrimServer(t, usage, large, small)

	rr := chat(t, srv, candidateTrimBody(40, 3000))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	rows := usage.Snapshot()
	var final *UsageRecord
	for i := range rows {
		if !rows[i].Attempt && !rows[i].Failed {
			final = &rows[i]
		}
	}
	if final == nil {
		t.Fatal("no successful usage row was recorded")
	}
	if !final.Trimmed {
		t.Fatalf("the successful row was not marked trimmed; rows=%+v", rows)
	}
	if final.ContextWindow != 20000 {
		t.Fatalf("ContextWindow = %d, want the serving candidate's 20000", final.ContextWindow)
	}
}

func TestImpossibleFirstCandidateFallsThroughToALargerOne(t *testing.T) {
	tiny := &windowClient{name: "tiny", window: 5000}
	large := &windowClient{name: "large", window: 1000000}
	srv := candidateTrimServer(t, NewUsageStore(10), tiny, large)

	body := `{"model":"m1","messages":[{"role":"user","content":"` + strings.Repeat("a", 40000) + `"}]}`
	rr := chat(t, srv, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if large.seen == nil {
		t.Fatal("the larger candidate was never tried after an impossible smaller one")
	}
}

func TestAllCandidatesImpossibleReturnsLocalContextError(t *testing.T) {
	tiny := &windowClient{name: "tiny", window: 5000}
	alsoTiny := &windowClient{name: "also-tiny", window: 5000}
	srv := candidateTrimServer(t, NewUsageStore(10), tiny, alsoTiny)

	body := `{"model":"m1","messages":[{"role":"user","content":"` + strings.Repeat("a", 40000) + `"}]}`
	rr := chat(t, srv, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want a local 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "context_window_exceeded") {
		t.Fatalf("the refusal lost its machine-readable code: %s", rr.Body.String())
	}
}

func TestRequestContextWindowHeaderTrimsForOneRequest(t *testing.T) {
	c := &windowClient{name: "solo", window: 1000000}
	usage := NewUsageStore(10)
	srv := candidateTrimServer(t, usage, c)

	body := candidateTrimBody(40, 3000)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client2API-Context-Window", "30000")
	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if c.seen == nil {
		t.Fatal("the module was never called")
	}
	if got := int64(estimateRequestTokens(c.seen)); got > 30000 {
		t.Fatalf("the one-off window did not trim the request: %d tokens", got)
	}
	if c.seen.ContextWindowLimit != 0 {
		t.Fatalf("the gateway-only window leaked to the module: %d", c.seen.ContextWindowLimit)
	}
	rows := usage.Snapshot()
	var final *UsageRecord
	for i := range rows {
		if !rows[i].Attempt && !rows[i].Failed {
			final = &rows[i]
		}
	}
	if final == nil || !final.Trimmed || final.ContextWindow != 30000 {
		t.Fatalf("usage row = %+v, want a trim recorded against 30000", final)
	}
}
