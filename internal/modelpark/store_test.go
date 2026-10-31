package modelpark

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// newTestStore returns a store with a clock the test can advance, so park
// durations are asserted without sleeping.
func newTestStore() (*Store, func(time.Duration)) {
	s := New()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, func(d time.Duration) { now = now.Add(d) }
}

func TestParkBacksOffFromSixtySeconds(t *testing.T) {
	s, _ := newTestStore()
	const (
		platform = "cline"
		account  = "acct-1"
		model    = "deepseek-v4.1-flash"
	)
	if got := s.Park(platform, account, model, "429", 0); got != 60*time.Second {
		t.Fatalf("first park = %v, want 60s", got)
	}
	if got := s.Park(platform, account, model, "429", 0); got != 120*time.Second {
		t.Fatalf("second park = %v, want 120s", got)
	}
}

func TestParkCapsAtThirtyMinutes(t *testing.T) {
	s, _ := newTestStore()
	var last time.Duration
	for i := 0; i < 10; i++ {
		last = s.Park("cline", "acct-1", "model-x", "429", 0)
	}
	if last != 30*time.Minute {
		t.Fatalf("capped park = %v, want 30m", last)
	}
}

func TestParkHonoursRetryAfter(t *testing.T) {
	s, _ := newTestStore()
	if got := s.Park("cline", "acct-1", "model-x", "429", 5*time.Minute); got != 5*time.Minute {
		t.Fatalf("park with Retry-After = %v, want 5m", got)
	}
	if got := s.Park("cline", "acct-2", "model-x", "429", 2*time.Hour); got != 30*time.Minute {
		t.Fatalf("park with an oversized Retry-After = %v, want the 30m cap", got)
	}
}

func TestActiveIgnoresALapsedPark(t *testing.T) {
	s, advance := newTestStore()
	s.Park("cline", "acct-1", "model-x", "429", 0)
	if _, ok := s.Active("cline", "acct-1", "model-x"); !ok {
		t.Fatal("a fresh park is not active")
	}
	advance(61 * time.Second)
	if _, ok := s.Active("cline", "acct-1", "model-x"); ok {
		t.Fatal("a lapsed park is still active")
	}
}

func TestForAccountListsOnlyThatAccountsLiveParks(t *testing.T) {
	s, _ := newTestStore()
	s.Park("cline", "acct-1", "model-b", "429", 0)
	s.Park("cline", "acct-1", "model-a", "429", 0)
	s.Park("cline", "acct-2", "model-a", "429", 0)

	parks := s.ForAccount("cline", "acct-1")
	if len(parks) != 2 {
		t.Fatalf("parks = %+v, want two for acct-1", parks)
	}
	if parks[0].Model != "model-a" || parks[1].Model != "model-b" {
		t.Fatalf("parks are not sorted by model: %+v", parks)
	}
	if parks[0].Kind != core.ModelParkRateLimit {
		t.Fatalf("park kind = %q, want %q", parks[0].Kind, core.ModelParkRateLimit)
	}
	if parks[0].Until == "" {
		t.Fatal("park has no release instant")
	}
}

func TestParkIgnoresEmptyIdentifiers(t *testing.T) {
	s, _ := newTestStore()
	if got := s.Park("cline", "", "model-x", "429", 0); got != 0 {
		t.Fatalf("park without an account = %v, want 0", got)
	}
	if got := s.Park("cline", "acct-1", "", "429", 0); got != 0 {
		t.Fatalf("park without a model = %v, want 0", got)
	}
}

// A park that actually lands must log one line naming the triple, the window,
// its source (vendor hint or local ladder) and the running hit count -- the
// Retry-After fix was unverifiable from the log until this existed (issue #18).
func TestParkLogsOneLineWhenLogfIsSet(t *testing.T) {
	s := New()
	var lines []string
	s.SetLogf(func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	// First hit with a vendor hint: the source must say retry_after.
	s.Park("raccoon", "key-1", "deepseek-v4", "HTTP 429: slow down", 90*time.Second)
	if len(lines) != 1 {
		t.Fatalf("lines = %d (%v), want exactly one", len(lines), lines)
	}
	for _, want := range []string{"raccoon/deepseek-v4", "parked 1m30s", "retry_after=1m30s", "hit #1", "HTTP 429"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line %q lacks %q", lines[0], want)
		}
	}
	// The account id is masked by core.MaskSecret: a short id becomes
	// "***len=N" and a raw key must never reach the log (issue #18).
	if strings.Contains(lines[0], "key-1") {
		t.Errorf("line %q leaks the raw account id", lines[0])
	}
	if !strings.Contains(lines[0], "***len=5") {
		t.Errorf("line %q lacks the masked-account marker", lines[0])
	}

	// Second hit inside the window with no hint: the ladder doubled, and the
	// line says so -- parks are not folded, the escalation is the information.
	s.Park("raccoon", "key-1", "deepseek-v4", "HTTP 429: slow down", 0)
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want two", len(lines))
	}
	if !strings.Contains(lines[1], "ladder") || !strings.Contains(lines[1], "hit #2") {
		t.Errorf("second line = %q, want ladder source and hit #2", lines[1])
	}
}

// Without a logger the store stays silent, as every existing caller built it.
func TestParkWithoutLogfIsSilent(t *testing.T) {
	s := New()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if d := s.Park("raccoon", "key-1", "m", "reason", time.Minute); d != time.Minute {
		t.Fatalf("park duration = %v, want 1m", d)
	}
}
