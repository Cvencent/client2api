package tabbit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestTabbitSidecarErrorsCarryGatewayFailureKinds(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   core.FailureKind
	}{
		{name: "auth", status: http.StatusUnauthorized, body: `{"error":{"message":"expired"}}`, want: core.FailureAuth},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":{"message":"denied"}}`, want: core.FailureAuth},
		{name: "rate", status: http.StatusTooManyRequests, body: `{"error":{"message":"slow down"}}`, want: core.FailureRateLimited},
		{name: "upstream", status: http.StatusBadGateway, body: `{"error":{"message":"bridge down"}}`, want: core.FailureUpstream},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return jsonResponse(tt.status, tt.body), nil
			})}
			clearTabbitEnv(t)
			c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)

			_, err := c.Chat(context.Background(), &core.ChatRequest{Model: "priority"})
			if err == nil {
				t.Fatal("Chat returned no error")
			}
			if got := core.FailureKindOf(err); got != tt.want {
				t.Fatalf("FailureKindOf(%v) = %q, want %q", err, got, tt.want)
			}
			if tt.want == core.FailureAuth && !errors.Is(err, core.ErrNotConfigured) {
				t.Fatalf("auth error = %v, want it to wrap core.ErrNotConfigured", err)
			}
		})
	}
}

func TestTabbitWebErrorsCarryGatewayFailureKinds(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   core.FailureKind
	}{
		{name: "auth", status: http.StatusUnauthorized, want: core.FailureAuth},
		{name: "forbidden", status: http.StatusForbidden, want: core.FailureAuth},
		{name: "rate", status: http.StatusTooManyRequests, want: core.FailureRateLimited},
		{name: "upstream", status: http.StatusBadGateway, want: core.FailureUpstream},
		{name: "other", status: http.StatusBadRequest, want: core.FailureOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := webError(webAuth{accountID: "tabbit-web:uid-1"}, "POST /run", tt.status, []byte(`{"message":"nope"}`))
			f, ok := core.AsFailure(err)
			if !ok {
				t.Fatalf("webError returned %T %v, want a core.Failure", err, err)
			}
			if f.Kind != tt.want || f.Client != "tabbit" || f.Account != "tabbit-web:uid-1" {
				t.Fatalf("failure = %+v, want kind=%q client=tabbit account=tabbit-web:uid-1", f, tt.want)
			}
		})
	}
}

func TestTabbitWebRateLimitRotatesToTheNextSession(t *testing.T) {
	f := newFakeWeb(t)
	c, first := newWebClient(t, f)

	secondToken := webJWTFor(t, "uid-web-0002", time.Now().Add(time.Hour))
	second := addEndpoint(t, c, map[string]string{"kind": kindWebToken, "token": secondToken})

	if ep, ok := c.pickWebSession("", "Default"); !ok || ep.ID != first.ID {
		t.Fatalf("initial picker = %q,%v, want the first session %q", ep.ID, ok, first.ID)
	}

	limited := core.Fail("tabbit", first.ID, core.FailureRateLimited, http.StatusTooManyRequests, errors.New("too many requests"))
	c.noteWebFailure(webAuth{accountID: first.ID}, limited)

	ep, ok := c.pickWebSession("", "Default")
	if !ok || ep.ID != second.ID {
		t.Fatalf("picker after 429 = %q,%v, want the next session %q", ep.ID, ok, second.ID)
	}
}

func TestTabbitChat429RecordsTheFailureAndRotatesSession(t *testing.T) {
	base := newFakeWeb(t)
	firstToken := webJWTFor(t, webTestUID, time.Now().Add(time.Hour))
	secondToken := webJWTFor(t, "uid-web-0002", time.Now().Add(time.Hour))

	var upstream http.Handler = base.Server.Config.Handler
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Cookie"), firstToken) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"slow down"}`))
			return
		}
		upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	clearTabbitEnv(t)
	c := newTestClient(t, `{"transport":"web","web_base_url":"`+server.URL+`"}`, server.Client())
	first := addEndpoint(t, c, map[string]string{"kind": kindWebToken, "token": firstToken})
	addEndpoint(t, c, map[string]string{"kind": kindWebToken, "token": secondToken})

	_, err := drainChat(context.Background(), c, &core.ChatRequest{
		Model:    "Default",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if got := core.FailureKindOf(err); got != core.FailureRateLimited {
		t.Fatalf("first Chat failure = %q, want %q: %v", got, core.FailureRateLimited, err)
	}
	if account := core.ErrorAccountID(err); account != first.ID {
		t.Fatalf("first Chat account = %q, want %q", account, first.ID)
	}

	reply, err := drainChat(context.Background(), c, &core.ChatRequest{
		Model:    "Default",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("second Chat after rotation: %v", err)
	}
	if reply != "WEBOK!" {
		t.Fatalf("second Chat reply = %q, want WEBOK!", reply)
	}
	if got := base.header().Get("Cookie"); !strings.Contains(got, secondToken) {
		t.Fatalf("second Chat Cookie = %q, want the second session", got)
	}
}

// TestTabbitChatWaitsOutACoolingSession pins the fix for the retry loop that
// kept answering the vendor's 429 with another run.  With a single session in
// the pool, a cooling account must report backpressure instead of creating
// another room and deepening the penalty.
func TestTabbitChatWaitsOutACoolingSession(t *testing.T) {
	f := newFakeWeb(t)
	c, rec := newWebClient(t, f)

	limited := core.Fail("tabbit", rec.ID, core.FailureRateLimited, http.StatusTooManyRequests,
		errors.New("Tabbit rate limited POST /api/v3/chat/rooms/x/runs (HTTP 429): slow down"))
	c.noteWebFailure(webAuth{accountID: rec.ID}, limited)

	_, err := drainChat(context.Background(), c, &core.ChatRequest{
		Model:    "Default",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, core.ErrBusy) {
		t.Fatalf("Chat on a cooling session = %v, want core.ErrBusy", err)
	}
	if n := f.hits("session"); n != 0 {
		t.Fatalf("a cooling session created %d room(s), want 0", n)
	}
}

// TestTabbitRateLimitHoldGrowsAcrossConsecutiveRefusals pins the backoff: a
// repeated 429 grows the rest and a completed run clears it.
func TestTabbitRateLimitHoldGrowsAcrossConsecutiveRefusals(t *testing.T) {
	f := newFakeWeb(t)
	c, rec := newWebClient(t, f)
	wa := webAuth{accountID: rec.ID}

	limited := core.Fail("tabbit", rec.ID, core.FailureRateLimited, http.StatusTooManyRequests,
		errors.New("Tabbit rate limited POST /api/v3/chat/rooms/x/runs (HTTP 429): slow down"))
	c.noteWebFailure(wa, limited)
	first, ok := c.webVerdictFor(rec.ID)
	if !ok || first.hold != webRateLimitCooldown {
		t.Fatalf("first 429 hold = %v, want %v", first.hold, webRateLimitCooldown)
	}

	c.noteWebFailure(wa, limited)
	second, _ := c.webVerdictFor(rec.ID)
	if want := 5 * time.Minute; second.hold != want {
		t.Fatalf("second 429 hold = %v, want %v", second.hold, want)
	}
	c.noteWebFailure(wa, limited)
	third, _ := c.webVerdictFor(rec.ID)
	if want := 10 * time.Minute; third.hold != want {
		t.Fatalf("third 429 hold = %v, want %v", third.hold, want)
	}
	if !c.webCooling(rec.ID, time.Now()) {
		t.Fatal("a grown hold did not keep the session cooling")
	}

	c.noteWebSuccess(wa)
	if c.webCooling(rec.ID, time.Now()) {
		t.Fatal("a completed run did not clear the rate-limit hold")
	}
}
