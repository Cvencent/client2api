package gateway

import (
	"errors"
	"strings"
	"testing"

	"client2api/internal/core"
)

// A platform that accepted the connection and then answered nothing is not a
// pool problem.  The reason the panel prints has to say so: on 2026-11-20 a
// wedged loomy upstream, with 33 ready accounts, produced "the platform has no
// usable account" -- which points the operator at the wrong fix entirely
// (sign in more accounts, when the vendor never answered anybody).
func TestFailureReasonSeparatesASilentUpstreamFromAnEmptyPool(t *testing.T) {
	stall := errors.Join(core.ErrPlatformExhausted, core.ErrUpstreamSilent,
		errors.New("loomy: chat: no response within 4m24s"))
	got := failureReason(stall)
	if !strings.Contains(got, "上游超时未响应") {
		t.Fatalf("failureReason(silent upstream) = %q, want it to name the stall", got)
	}
	if strings.Contains(got, "平台暂无可用账号") {
		t.Fatalf("failureReason(silent upstream) = %q, must not claim the pool is empty", got)
	}
	// The exhausted sentinel on its own keeps its own wording: a module that
	// walked the pool and gave up says exactly that.
	if got := failureReason(core.ErrPlatformExhausted); !strings.Contains(got, "平台暂无可用账号") {
		t.Fatalf("failureReason(ErrPlatformExhausted) = %q, want the pool wording kept", got)
	}
}
