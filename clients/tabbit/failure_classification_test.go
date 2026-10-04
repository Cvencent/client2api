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
