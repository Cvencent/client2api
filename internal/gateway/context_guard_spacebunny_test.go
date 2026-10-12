package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

// TestSpaceBunnyInflatedWindowIsCorrected pins the reported bug end to end,
// using the real platform and model ids.
//
// The catalogue publishes cn:space-bunny with a 1,000,000-token window while the
// vendor enforces 100,000.  A client that sizes its packing from /v1/models
// therefore builds a request the vendor will always refuse.  Once the vendor's
// own number has been learned, the listing has to report the window the vendor
// actually serves -- otherwise the recurrence is only delayed, not prevented.
func TestSpaceBunnyInflatedWindowIsCorrected(t *testing.T) {
	c := &bunnyClient{name: "workbuddy", realWindow: 100000}
	s := newGuardServer(t, c, filepath.Join(t.TempDir(), "learned_context_windows.json"))

	// Before: the listing carries the inflated figure.
	body := modelsBody(t, s)
	if !strings.Contains(body, "1000000") {
		t.Fatalf("expected the inflated 1000000 window before learning, got %s", clip(body))
	}

	// The vendor refuses and names its real limit.
	refusal := core.Fail("workbuddy", "", core.FailureContextWindow, 400,
		errTooLong(100001, 100000))
	if got := s.learnContextWindowFromFailure(context.Background(), c, "cn:space-bunny", refusal); got != 100000 {
		t.Fatalf("expected to learn 100000, got %d", got)
	}

	// After: the listing must carry the vendor's number in both places, because
	// clients read either the top-level field or extra.
	body = modelsBody(t, s)
	if strings.Contains(body, `"context_length":1000000`) {
		t.Fatalf("the inflated window is still advertised: %s", clip(body))
	}
	if !strings.Contains(body, `"context_length":100000`) {
		t.Fatalf("the learned window must be advertised: %s", clip(body))
	}
	if strings.Count(body, `"context_length":1000000`) != 0 {
		t.Fatalf("extra still leaks the inflated value: %s", clip(body))
	}
}

// TestSpaceBunnyRequestIsTrimmedAfterLearning is the follow-on behaviour: once
// the window is known, the next oversized request is served rather than refused.
func TestSpaceBunnyRequestIsTrimmedAfterLearning(t *testing.T) {
	c := &bunnyClient{name: "workbuddy", realWindow: 100000}
	s := newGuardServer(t, c, filepath.Join(t.TempDir(), "learned_context_windows.json"))
	s.learnedWindows.put("workbuddy", "cn:space-bunny", 100000)

	body := strings.Replace(oversizedBody(80, 4000), `"bunny/cn:space-bunny"`, `"workbuddy/cn:space-bunny"`, 1)
	rr := chatHandler(t, s, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected the request to be trimmed and served, got %d: %s", rr.Code, rr.Body.String())
	}
	if c.seen == nil || int64(estimateRequestTokens(c.seen)) > 100000 {
		t.Fatalf("the module must receive a request that fits the real window")
	}
}

// errTooLong renders the vendor's own refusal shape.
func errTooLong(got, max int64) error {
	return &tooLongError{got: got, max: max}
}

type tooLongError struct{ got, max int64 }

func (e *tooLongError) Error() string {
	return fmt.Sprintf(
		"upstream prompt_too_long (http 400): {\"code\":11115,\"msg\":\"prompt is too long: %d tokens > %d maximum\"}",
		e.got, e.max)
}

func modelsBody(t *testing.T, s *server) string {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d", rr.Code)
	}
	return rr.Body.String()
}
