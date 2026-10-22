package gateway

import (
	"context"
	"strings"
	"testing"

	"client2api/internal/core"
)

// probeMultiTurn builds a request with enough separate turns that the guard can
// actually drop older ones.  A single oversized message can never be trimmed,
// so the guard would refuse it as impossible and the compaction question would
// never arise.
func probeMultiTurn(tokensPerTurn, turns int) *core.ChatRequest {
	msgs := make([]core.Message, 0, turns)
	for i := 0; i < turns; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, core.Message{Role: role, Content: strings.Repeat("一", tokensPerTurn)})
	}
	return &core.ChatRequest{Messages: msgs}
}

// A model that has already been probed and REFUSED must still let the Responses
// compaction path run.  probeProceed means "the normal budget logic owns this",
// and that logic trims -- so returning "no compaction" for it would recreate
// the silent-trim loop compaction exists to prevent.
func TestResponsesWouldTrimStillTrueAfterARefusedProbe(t *testing.T) {
	srv := probeTestServer(t, 1_000_000, "")
	srv.probes.recordRefused("fake", "cn:space-bunny", 1_050_000)

	req := probeMultiTurn(35000, 30)
	req.Model = "fake/cn:space-bunny"
	if !srv.responsesWouldTrim(context.Background(), req) {
		t.Fatal("a refused probe leaves the model on the trim path; compaction must still fire")
	}
}

// A never-probed model is probed before anything is folded: the probe both
// costs less than a summarisation call and teaches the real window.
func TestResponsesWouldTrimFalseBeforeAProbe(t *testing.T) {
	srv := probeTestServer(t, 1_000_000, "")

	req := probeMultiTurn(35000, 30)
	req.Model = "fake/cn:space-bunny"
	if srv.responsesWouldTrim(context.Background(), req) {
		t.Fatal("a never-probed model must be probed, not folded, on the first over-budget turn")
	}
}

// An accepted floor turns the same request into a normal send: no trim, and
// therefore no compaction item.
func TestResponsesWouldTrimFalseInsideAnAcceptedFloor(t *testing.T) {
	srv := probeTestServer(t, 1_000_000, "")
	req := probeMultiTurn(35000, 30)
	req.Model = "fake/cn:space-bunny"
	// The floor must cover the exact estimate, or the request is genuinely
	// above the proven size and trimming is correct.
	srv.probes.recordAccepted("fake", "cn:space-bunny", int64(estimateRequestTokens(req)), 900_000)
	if srv.responsesWouldTrim(context.Background(), req) {
		t.Fatal("a request under the accepted floor is not trimmed, so it must not be compacted")
	}
}
