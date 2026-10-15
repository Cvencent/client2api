package gateway

import (
	"errors"
	"io"
	"log"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/livecfg"
)

// A vendor that reports an in-stream quota error on a 200 SSE stream must not
// fail the whole request: the gateway has not sent any bytes to the caller yet,
// so it may close that stream and try the next platform.
func TestChatFailsOverFromInStreamQuotaError(t *testing.T) {
	exhausted := &testClient{
		name: "exhausted",
		events: []core.Event{
			{Type: core.EventError, Err: core.Fail("exhausted", "a1", core.FailureQuota, http.StatusBadGateway, errors.New("免费额度已用完，请升级套餐"))},
		},
	}
	healthy := &testClient{
		name:     "healthy",
		servedBy: "h1",
		events:   usageEvents(3, 4),
	}

	reg := core.NewRegistry()
	reg.Add(exhausted)
	reg.Add(healthy)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"exhausted": {Priority: 1},
		"healthy":   {Priority: 2},
	})

	usage := NewUsageStore(10)
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    usage,
		Live: livecfg.New(livecfg.Snapshot{
			MaxRotate:         1,
			RotateBackoffBase: time.Nanosecond,
		}),
	})

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if healthy.seen == nil {
		t.Fatal("the healthy platform never received the request after the in-stream quota error")
	}
}

// The same rule holds for every protocol, so it is pinned on Responses too:
// the probe must live in the shared candidate loop, not in one wire renderer.
func TestResponsesFailsOverFromInStreamQuotaError(t *testing.T) {
	exhausted := &testClient{
		name: "exhausted",
		events: []core.Event{
			{Type: core.EventError, Err: core.Fail("exhausted", "a1", core.FailureQuota, http.StatusBadGateway, errors.New("free quota exhausted"))},
		},
	}
	healthy := &testClient{
		name:     "healthy",
		servedBy: "h1",
		events:   usageEvents(3, 4),
	}

	reg := core.NewRegistry()
	reg.Add(exhausted)
	reg.Add(healthy)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"exhausted": {Priority: 1},
		"healthy":   {Priority: 2},
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

	rec := responses(t, srv, `{"model":"m1","stream":true,"input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if healthy.seen == nil {
		t.Fatal("the healthy platform never received the request after the in-stream quota error")
	}
}
