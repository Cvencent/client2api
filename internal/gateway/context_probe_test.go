package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProbeStoreRecordsAcceptedAndRaisesTheFloor(t *testing.T) {
	s := openProbeStore(filepath.Join(t.TempDir(), "probes.json"), t.Logf)

	if !s.recordAccepted("raccoon", "sn-deepseek-v4-1-flash-1017", 1_043_449, 961_759) {
		t.Fatal("first accepted record must report a change")
	}
	got, ok := s.get("raccoon", "sn-deepseek-v4-1-flash-1017")
	if !ok {
		t.Fatal("accepted record must be found")
	}
	if got.Outcome != probeOutcomeAccepted {
		t.Fatalf("outcome = %q, want %q", got.Outcome, probeOutcomeAccepted)
	}
	if got.EstimateTokens != 1_043_449 {
		t.Fatalf("floor = %d, want 1043449", got.EstimateTokens)
	}
	if got.VendorTokens != 961_759 {
		t.Fatalf("vendor tokens = %d, want 961759", got.VendorTokens)
	}
	if !got.Probed {
		t.Fatal("record must be marked probed")
	}

	// A smaller accepted size must not lower the floor.
	s.recordAccepted("raccoon", "sn-deepseek-v4-1-flash-1017", 900_000, 850_000)
	got, _ = s.get("raccoon", "sn-deepseek-v4-1-flash-1017")
	if got.EstimateTokens != 1_043_449 {
		t.Fatalf("floor after smaller accept = %d, want it unchanged at 1043449", got.EstimateTokens)
	}

	// A larger accepted size must raise it.
	s.recordAccepted("raccoon", "sn-deepseek-v4-1-flash-1017", 1_200_000, 1_100_000)
	got, _ = s.get("raccoon", "sn-deepseek-v4-1-flash-1017")
	if got.EstimateTokens != 1_200_000 {
		t.Fatalf("floor after larger accept = %d, want 1200000", got.EstimateTokens)
	}
}

func TestProbeStoreRefusedHasNoFloor(t *testing.T) {
	s := openProbeStore(filepath.Join(t.TempDir(), "probes.json"), t.Logf)
	s.recordRefused("raccoon", "sn-deepseek-v4-1-flash-1017", 1_043_449)

	got, ok := s.get("raccoon", "sn-deepseek-v4-1-flash-1017")
	if !ok {
		t.Fatal("refused record must be found")
	}
	if got.Outcome != probeOutcomeRefused {
		t.Fatalf("outcome = %q, want %q", got.Outcome, probeOutcomeRefused)
	}
	if got.EstimateTokens != 0 {
		t.Fatalf("refused record must carry no floor, got %d", got.EstimateTokens)
	}
}

func TestProbeStoreClearRemovesOnlyThatEntry(t *testing.T) {
	s := openProbeStore(filepath.Join(t.TempDir(), "probes.json"), t.Logf)
	s.recordAccepted("raccoon", "model-a", 100, 90)
	s.recordAccepted("raccoon", "model-b", 200, 180)

	if !s.clear("raccoon", "model-a") {
		t.Fatal("clear must report a change for a present entry")
	}
	// A clear leaves a tombstone rather than deleting the entry: a model whose
	// context length is known would otherwise look settled again immediately.
	if rec, ok := s.get("raccoon", "model-a"); !ok || rec.Probed || rec.Outcome != probeOutcomeCleared {
		t.Fatalf("after clear = %+v ok=%v, want a cleared tombstone", rec, ok)
	}
	if _, ok := s.get("raccoon", "model-b"); !ok {
		t.Fatal("sibling entry must survive")
	}
	if s.clear("raccoon", "model-a") {
		t.Fatal("clearing an absent entry must report no change")
	}
}

func TestProbeStoreSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probes.json")
	s := openProbeStore(path, t.Logf)
	s.recordAccepted("raccoon", "model-a", 1_043_449, 961_759)

	reloaded := openProbeStore(path, t.Logf)
	got, ok := reloaded.get("raccoon", "model-a")
	if !ok {
		t.Fatal("record must survive a reload")
	}
	if got.EstimateTokens != 1_043_449 || got.VendorTokens != 961_759 {
		t.Fatalf("reloaded record = %+v, want the accepted values", got)
	}
}

func TestProbeStoreCorruptFileCollapsesToEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probes.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openProbeStore(path, t.Logf)
	if _, ok := s.get("raccoon", "model-a"); ok {
		t.Fatal("corrupt store must read as empty")
	}
	if !s.recordAccepted("raccoon", "model-a", 500, 400) {
		t.Fatal("a corrupt store must still accept new records")
	}
	if got, ok := s.get("raccoon", "model-a"); !ok || got.EstimateTokens != 500 {
		t.Fatalf("record after corruption = %+v ok=%v, want floor 500", got, ok)
	}
}
