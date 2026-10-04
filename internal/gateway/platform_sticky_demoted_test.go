package gateway

import (
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/livecfg"
)

// A conversation that was bound to a platform must not keep pinning it after
// that platform has been demoted for being unavailable.  The binding is an
// optimisation; once the pinned platform is known to be unable to serve, the
// healthy candidate should win and the binding is replaced by whoever answers.
func TestChatDoesNotPinADemotedPlatformForStickyConversations(t *testing.T) {
	alpha := &testClient{name: "alpha", servedBy: "a1", events: usageEvents(1, 1)}
	gamma := &testClient{name: "gamma", servedBy: "g1", events: usageEvents(3, 4)}

	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(gamma)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 1},
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

	scoped := `{"model":"m1","conversation_id":"c1","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, scoped); rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s", rec.Code, rec.Body.String())
	}
	if alpha.seen == nil {
		t.Fatal("alpha never served the first scoped request")
	}

	// alpha becomes saturated.  An unscoped request carries no conversation
	// key, so it cannot rebind c1; it only demotes alpha.
	alpha.chatErr = core.ErrBusy
	if rec := chat(t, srv, bufferedBody); rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, body %s", rec.Code, rec.Body.String())
	}
	if !reg.ModelDegraded("alpha", "m1", time.Now()) {
		t.Fatal("alpha was not demoted after reporting busy")
	}

	alpha.seen = nil
	gamma.seen = nil
	if rec := chat(t, srv, scoped); rec.Code != http.StatusOK {
		t.Fatalf("third status = %d, body %s", rec.Code, rec.Body.String())
	}
	if alpha.seen != nil {
		t.Fatal("the sticky binding pinned the demoted alpha platform")
	}
	if gamma.seen == nil {
		t.Fatal("the healthy platform never received the scoped request")
	}
	if !strings.Contains(gamma.seen.Model, "m1") {
		t.Fatalf("gamma model = %q, want m1", gamma.seen.Model)
	}
}
