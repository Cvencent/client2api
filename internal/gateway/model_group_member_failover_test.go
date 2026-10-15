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

// A bare group name is the operator's failover chain: if its first member
// answers "this model does not exist here", the gateway must fall through to
// the other members of the same group.
func TestChatFailsOverFromGroupMemberRefusal(t *testing.T) {
	alpha := &testClient{
		name:      "alpha",
		catalogue: []core.Model{{ID: "alpha-model"}},
		chatErr:   core.Fail("alpha", "a1", core.FailureOther, http.StatusNotFound, errors.New("model not found")),
	}
	beta := &testClient{
		name:      "beta",
		catalogue: []core.Model{{ID: "beta-model"}},
		servedBy:  "b1",
		events:    usageEvents(3, 4),
	}

	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 1},
		"beta":  {Priority: 2},
	})
	reg.SetModelGroups(map[string]core.ModelGroup{
		"shared": {
			Members: []core.ModelGroupMember{
				{Client: "alpha", Model: "alpha-model"},
				{Client: "beta", Model: "beta-model"},
			},
		},
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

	rec := chat(t, srv, `{"model":"shared","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if beta.seen == nil {
		t.Fatal("the sibling group member never received the failover")
	}

	// An explicit platform choice is answered by that platform: the group is not
	// allowed to override the caller, even when the named member refuses.
	rec = chat(t, srv, `{"model":"alpha/alpha-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("explicit alpha status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}
