package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/livecfg"
)

// realmTestClient is a qoder-shaped module: it publishes realm-qualified model
// ids and fails the international one with the exact transport error the real
// module produces when the vendor's HTTPDNS-only host cannot be resolved.
type realmTestClient struct {
	seen     []string // every model id Chat was asked for
	failOn   string   // the model id that fails
	failWith error
}

func (c *realmTestClient) Name() string { return "qoder" }

func (c *realmTestClient) Models(context.Context) ([]core.Model, error) {
	return []core.Model{
		{ID: "cn:auto", OwnedBy: "qoder"},
		{ID: "global:auto", OwnedBy: "qoder"},
	}, nil
}

func (c *realmTestClient) Chat(_ context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.seen = append(c.seen, req.Model)
	if req.Model == c.failOn {
		return nil, c.failWith
	}
	return &fakeStream{events: []core.Event{
		{Type: core.EventDelta, Delta: "hello"},
		{Type: core.EventUsage, Usage: &core.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}},
		{Type: core.EventDone, Finish: "stop"},
	}}, nil
}

func (c *realmTestClient) Status(context.Context) core.Status {
	return core.Status{Name: "qoder", Ready: true}
}

// A bare "auto" request must fail over from the international realm's
// unresolvable model host to the domestic candidate, not end the whole route.
// This is the operator's exact worry: an international account whose chat
// cannot connect must not take down a request a domestic account can serve.
func TestRealmBareModelFailsOverFromAnUnresolvableInternationalHost(t *testing.T) {
	// The real error shape: a *url.Error wrapping the DNS failure, which is what
	// net/http reports for an unknown host.
	dnsFailure := &url.Error{Op: "Post", URL: "https://api2-v2.qoder.sh/model/v1/chat/completions",
		Err: fmt.Errorf("dial tcp: lookup api2-v2.qoder.sh: no such host")}

	client := &realmTestClient{failOn: "global:auto", failWith: dnsFailure}

	reg := core.NewRegistry()
	reg.Add(client)
	reg.SetModelGroups(map[string]core.ModelGroup{
		"auto-group": {
			Members: []core.ModelGroupMember{
				{Client: "qoder", Model: "global:auto"},
				{Client: "qoder", Model: "cn:auto"},
			},
		},
	})

	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
		Live: livecfg.New(livecfg.Snapshot{
			MaxRotate:         1,
			RotateBackoffBase: time.Nanosecond,
		}),
	})

	rec := chat(t, srv, `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s: the unresolvable international host must fail over, not fail the request",
			rec.Code, rec.Body.String())
	}
	t.Logf("chat saw models in order: %v", client.seen)
	if len(client.seen) < 2 {
		t.Fatalf("chat was called %d times (%v); the domestic candidate was never tried", len(client.seen), client.seen)
	}
	if client.seen[0] != "global:auto" {
		t.Fatalf("first attempt = %q, want the group's first member global:auto", client.seen[0])
	}
	sawCN := false
	for _, m := range client.seen {
		if m == "cn:auto" {
			sawCN = true
		}
	}
	if !sawCN {
		t.Fatalf("the domestic candidate cn:auto was never tried: %v", client.seen)
	}
}
