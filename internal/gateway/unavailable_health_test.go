package gateway

import (
	"io"
	"log"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// healthBackedClient lets a fake module expose the same account-pool verdict a
// real module publishes through core.HealthProvider.
type healthBackedClient struct {
	*testClient
	health core.Health
}

func (c *healthBackedClient) Health() core.Health { return c.health }

// A hard-pinned platform is still allowed to say "I cannot serve this right
// now" before Chat is entered.  The caller must not make the gateway run the
// same empty account rotation once per inbound retry.
func TestUnavailablePinnedPlatformDoesNotCallChatOnEveryRequest(t *testing.T) {
	base := &testClient{
		name:      "workbuddy",
		catalogue: []core.Model{{ID: "global:deepseek-v4.1-flash"}},
		chatErr:   core.ErrPlatformExhausted,
	}
	client := &healthBackedClient{
		testClient: base,
		health: core.Health{
			Servable: false,
			Cooling:  2,
			Total:    2,
			Note:     "no account is usable right now",
		},
	}

	reg := core.NewRegistry()
	reg.Add(client)
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
	})

	body := `{"model":"workbuddy/global:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		rec := chat(t, srv, body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("request %d status = %d, body %s", i+1, rec.Code, rec.Body.String())
		}
		if base.seen != nil {
			t.Fatalf("request %d entered Chat even though the module reported no usable account", i+1)
		}
		if !strings.Contains(rec.Body.String(), `"code":"no_healthy_account"`) {
			t.Fatalf("request %d body = %s, want no_healthy_account", i+1, rec.Body.String())
		}
	}
}

// An exhausted platform is local unavailability, not an upstream 502.  A 503
// with Retry-After tells a well-behaved caller to back off instead of starting
// another immediate retry storm.
func TestPlatformExhaustedReturnsServiceUnavailableWithRetryAfter(t *testing.T) {
	client := &testClient{
		name:      "workbuddy",
		catalogue: []core.Model{{ID: "global:deepseek-v4.1-flash"}},
		chatErr:   core.ErrPlatformExhausted,
	}
	reg := core.NewRegistry()
	reg.Add(client)
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
	})

	body := `{"model":"workbuddy/global:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	rec := chat(t, srv, body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	// issue #155 之后：无冷却时刻可用的 503 不再携带写死的 30——那会把内部
	// 跳过窗口说成调用方该等的时长。缺省（无头）比一个错的短值安全。
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want NO header（无提示时宁缺勿错，issue #155）", got)
	}
	if !strings.Contains(rec.Body.String(), `"code":"no_healthy_account"`) {
		t.Fatalf("body = %s, want no_healthy_account", rec.Body.String())
	}
}

// Health is account-level, while a free model has one deliberate exception:
// a credit-parked account may still serve it.  The gateway must let Chat make
// that model-aware decision instead of treating the generic health verdict as
// final.
func TestUnavailableHealthDoesNotBlockFreeModel(t *testing.T) {
	base := &testClient{
		name: "workbuddy",
		catalogue: []core.Model{{
			ID:    "free-model",
			Extra: map[string]any{"free": true},
		}},
		servedBy: "wb-1",
		events:   usageEvents(1, 1),
	}
	client := &healthBackedClient{
		testClient: base,
		health: core.Health{
			Servable: false,
			Total:    1,
			Note:     "account is parked for credit exhaustion",
		},
	}
	reg := core.NewRegistry()
	reg.Add(client)
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
	})

	body := `{"model":"workbuddy/free-model","messages":[{"role":"user","content":"hi"}]}`
	rec := chat(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if base.seen == nil {
		t.Fatal("the free model never entered Chat")
	}
}
