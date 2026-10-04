package livecfg

import (
	"sync"
	"testing"
	"time"
)

func TestStoreAndLoadRoundTrip(t *testing.T) {
	// Every field has to survive, because a dropped one silently reverts the
	// panel to a default an operator did not choose.
	want := Snapshot{
		APIKey:               "deadbeefdeadbeef",
		SoftCooldown:         600 * time.Second,
		SanitizeFingerprints: true,
		PromptMode:           "append",
		PromptFile:           "/tmp/custom.md",
		MaxRotate:            3,
		RotateBackoffBase:    750 * time.Millisecond,
	}
	h := New(want)
	if got := h.Load(); got != want {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}

func TestAuthEnabledIsDerivedFromTheKey(t *testing.T) {
	// The panel must never be able to report "auth on" while the empty key
	// lets everything through, so this is derived rather than stored.
	cases := []struct {
		name string
		key  string
		want bool
	}{
		{"a key enforces auth", "deadbeef", true},
		{"an empty key disables auth", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Snapshot{APIKey: tc.key}
			if got := s.AuthEnabled(); got != tc.want {
				t.Fatalf("AuthEnabled() = %v, want %v", got, tc.want)
			}
			if got := New(s).Load().AuthEnabled(); got != tc.want {
				t.Fatalf("through the holder: AuthEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUpdateRewritesACopy(t *testing.T) {
	// The point of the copy: a reader that loaded the old snapshot keeps a
	// consistent view, so an in-flight request cannot observe a half-applied
	// change.
	h := New(Snapshot{APIKey: "first", MaxRotate: 1})
	before := h.Load()

	h.Update(func(s *Snapshot) {
		s.APIKey = "second"
		s.MaxRotate = 5
		s.PromptMode = "custom"
	})

	if before.APIKey != "first" || before.MaxRotate != 1 || before.PromptMode != "" {
		t.Fatalf("the earlier snapshot changed underneath its reader: %+v", before)
	}
	after := h.Load()
	if after.APIKey != "second" || after.MaxRotate != 5 || after.PromptMode != "custom" {
		t.Fatalf("Update did not persist its changes: %+v", after)
	}
}

func TestUpdateFromAZeroSnapshotTouchesOnlyWhatItNames(t *testing.T) {
	// Update starts from Load(), so a Holder that was never stored behaves as
	// though it held the zero snapshot rather than panicking on a nil pointer.
	h := &Holder{}
	h.Update(func(s *Snapshot) { s.SoftCooldown = 90 * time.Second })

	got := h.Load()
	if got.SoftCooldown != 90*time.Second {
		t.Fatalf("SoftCooldown = %v, want 90s", got.SoftCooldown)
	}
	if got.PromptMode != "" || got.APIKey != "" {
		t.Fatalf("untouched fields were invented: %+v", got)
	}
}

func TestNilHolderIsInert(t *testing.T) {
	// Callers must never have to nil-check: the gateway is constructed in
	// tests without a holder, and a panic there would be a wiring accident
	// rather than a configuration error.
	var h *Holder
	if got := h.Load(); got != (Snapshot{}) {
		t.Fatalf("nil Load() = %+v, want the zero snapshot", got)
	}
	h.Store(Snapshot{APIKey: "ignored"})
	h.Update(func(s *Snapshot) { s.APIKey = "also ignored" })
	if got := h.Load(); got != (Snapshot{}) {
		t.Fatalf("a nil holder started holding state: %+v", got)
	}
}

func TestUpdateWithNoFunctionIsANoOp(t *testing.T) {
	// A nil callback is how a caller says "nothing to change"; it must not
	// clear the snapshot it was about to leave alone.
	h := New(Snapshot{APIKey: "keep", PromptMode: "custom"})
	h.Update(nil)
	if got := h.Load(); got.APIKey != "keep" || got.PromptMode != "custom" {
		t.Fatalf("a nil callback disturbed the snapshot: %+v", got)
	}
}

func TestAStoreIsVisibleToEveryReader(t *testing.T) {
	// The whole reason for the atomic pointer: the panel writes a new
	// snapshot while request goroutines read the old one, with no lock.
	h := New(Snapshot{APIKey: "a"})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if s := h.Load(); s.APIKey == "" {
					t.Errorf("a reader saw an empty key while one was always set")
					return
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		h.Store(Snapshot{APIKey: "b", MaxRotate: i})
	}
	wg.Wait()

	if got := h.Load(); got.APIKey != "b" || got.MaxRotate != 99 {
		t.Fatalf("the last store did not win: %+v", got)
	}
}
