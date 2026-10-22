package gateway

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

func probeTestClient(t *testing.T, window int64) *catalogueClient {
	t.Helper()
	return &catalogueClient{name: "fake", window: window}
}

func probeTestServer(t *testing.T, window int64, learnedPath string) *server {
	t.Helper()
	s := newGuardServer(t, probeTestClient(t, window), learnedPath, func(o *Options) {
		o.ProbeBeforeTrim = true
	})
	s.probes = openProbeStore(filepath.Join(t.TempDir(), "probes.json"), t.Logf)
	return s
}

// probeTestRequest builds a request whose estimate is exactly want tokens.
// estimateRequestTokens charges 3 for the envelope, 4 per message, and one
// token per CJK character, so a run of wide runes is a precise stand-in.
func probeTestRequest(want int) *core.ChatRequest {
	body := strings.Repeat("一", want-3-messageOverheadTokens)
	return &core.ChatRequest{Messages: []core.Message{
		{Role: "user", Content: body},
	}}
}

func TestProbeDecisionForwardsWholeWhenNeverProbed(t *testing.T) {
	srv := probeTestServer(t, 1_000_000, "")
	req := probeTestRequest(1_050_000)
	if got := srv.probeDecision(context.Background(), req, probeTestClient(t, 1_000_000), "cn:space-bunny", 0, 0); got != probeForwardWhole {
		t.Fatalf("verdict = %v, want probeForwardWhole", got)
	}
}

func TestProbeDecisionUsesAcceptedFloor(t *testing.T) {
	srv := probeTestServer(t, 1_000_000, "")
	srv.probes.recordAccepted("fake", "cn:space-bunny", 1_043_449, 961_759)
	req := probeTestRequest(1_043_449)
	if got := srv.probeDecision(context.Background(), req, probeTestClient(t, 1_000_000), "cn:space-bunny", 0, 0); got != probeProceed {
		t.Fatalf("verdict = %v, want probeProceed inside the accepted floor", got)
	}
}

func TestProbeDecisionAboveFloorStillTrims(t *testing.T) {
	srv := probeTestServer(t, 1_000_000, "")
	srv.probes.recordAccepted("fake", "cn:space-bunny", 1_043_449, 961_759)
	req := probeTestRequest(1_400_000)
	if got := srv.probeDecision(context.Background(), req, probeTestClient(t, 1_000_000), "cn:space-bunny", 0, 0); got != probeTrimExceeded {
		t.Fatalf("verdict = %v, want probeTrimExceeded above the accepted floor", got)
	}
}

func TestProbeDecisionAlreadyRefusedDoesNotProbe(t *testing.T) {
	srv := probeTestServer(t, 1_000_000, "")
	srv.probes.recordRefused("fake", "cn:space-bunny", 1_050_000)
	req := probeTestRequest(1_050_000)
	if got := srv.probeDecision(context.Background(), req, probeTestClient(t, 1_000_000), "cn:space-bunny", 0, 0); got != probeProceed {
		t.Fatalf("verdict = %v, want probeProceed once refused", got)
	}
}

func TestProbeDecisionUnknownWindowNeverProbes(t *testing.T) {
	srv := probeTestServer(t, 0, "")
	req := probeTestRequest(1_050_000)
	if got := srv.probeDecision(context.Background(), req, probeTestClient(t, 0), "cn:space-bunny", 0, 0); got != probeProceed {
		t.Fatalf("verdict = %v, want probeProceed when the window is unknown", got)
	}
}

// There is no "too far over" cutoff: the estimator is the thing under test, so
// even a request it rejects by a wide margin still goes to the vendor first.
func TestProbeDecisionHasNoHardCeiling(t *testing.T) {
	srv := probeTestServer(t, 1_000_000, "")
	req := probeTestRequest(5_000_000)
	if got := srv.probeDecision(context.Background(), req, probeTestClient(t, 1_000_000), "cn:space-bunny", 0, 0); got != probeForwardWhole {
		t.Fatalf("verdict = %v, want probeForwardWhole even far above the window", got)
	}
}
