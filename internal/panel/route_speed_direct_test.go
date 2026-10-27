package panel

import (
	"testing"
	"time"

	"client2api/internal/core"
)

func TestDebugSpeedRouteStepDirect(t *testing.T) {
	slowEvents := []timedEvent{
		{ev: core.Event{Type: core.EventDelta, Delta: "ok"}, wait: 400 * time.Millisecond},
		{ev: core.Event{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: 2, TotalTokens: 12}}},
		{ev: core.Event{Type: core.EventDone}},
	}
	slow := &routeSpeedClient{
		fakeClient:         &fakeClient{name: "slow", models: []core.Model{{ID: "gpt-x"}}},
		account:            "slow-account",
		chatStreamOverride: &timedStream{events: slowEvents, logf: t.Logf},
	}
	reg := registryOf(slow)
	p := &panel{opts: Options{Registry: reg}}
	step := core.RoutePlanStep{Position: 1, Platform: "slow", ModelID: "gpt-x"}
	res := p.speedRouteStep(t.Context(), step)
	t.Logf("res: ok=%v first=%dms elapsed=%dms tokens=%d rate=%.3f err=%q",
		res.OK, res.FirstEventMS, res.ElapsedMS, res.CompletionTokens, res.TokensPerSecond, res.Error)
}
