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

// TestChatDoesNotRetryUnavailablePlatformOnEveryRequest pins the fix for the
// bare-model failover path: once a platform has said "I have no usable account
// right now", the next request must prefer the healthy candidate instead of
// paying for that platform again.  The demoted platform stays in the list, so a
// later request still uses it if every healthy platform fails.
func TestChatDoesNotRetryUnavailablePlatformOnEveryRequest(t *testing.T) {
	busy := &testClient{name: "busy", chatErr: core.ErrBusy}
	gamma := &testClient{name: "gamma", servedBy: "g1", events: usageEvents(3, 4)}

	reg := core.NewRegistry()
	reg.Add(busy)
	reg.Add(gamma)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"busy":  {Priority: 1},
		"gamma": {Priority: 2},
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

	first := chat(t, srv, bufferedBody)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s", first.Code, first.Body.String())
	}
	if busy.seen == nil {
		t.Fatal("the busy platform was never tried on the first request")
	}

	busy.seen = nil
	second := chat(t, srv, bufferedBody)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body %s", second.Code, second.Body.String())
	}
	if busy.seen != nil {
		t.Fatal("the demoted busy platform was tried again on the second request")
	}
	if gamma.seen == nil {
		t.Fatal("the healthy platform never received the second request")
	}

	got := usage.Snapshot()
	var attempts int
	for i := range got {
		if got[i].Attempt && got[i].Client == "busy" && got[i].Failed {
			attempts++
		}
	}
	if attempts != 1 {
		t.Fatalf("busy candidate attempts = %d, want exactly one after the demotion", attempts)
	}
}

// A platform that is genuinely broken still needs the short demotion to expire:
// the candidate list keeps it, so it must be retried once the window elapses.
func TestChatRetriesUnavailablePlatformAfterDemotionExpires(t *testing.T) {
	busy := &testClient{name: "busy", chatErr: core.ErrBusy}
	gamma := &testClient{name: "gamma", servedBy: "g1", events: usageEvents(3, 4)}

	reg := core.NewRegistry()
	reg.Add(busy)
	reg.Add(gamma)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"busy":  {Priority: 1},
		"gamma": {Priority: 2},
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

	if rec := chat(t, srv, bufferedBody); rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s", rec.Code, rec.Body.String())
	}

	// Expire the demotion the way wall-clock time would, without sleeping.
	reg.NoteModelUnavailable("busy", "m1", time.Now().Add(-2*core.PlatformUnavailableCooldown))
	busy.seen = nil
	// A different message is a different conversation, so platform stickiness
	// from the first request must not hide the expired demotion.
	other := `{"model":"m1","messages":[{"role":"user","content":"different conversation"}]}`
	if rec := chat(t, srv, other); rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, body %s", rec.Code, rec.Body.String())
	}
	if busy.seen == nil {
		t.Fatal("the busy platform was not retried after the demotion expired")
	}
}
