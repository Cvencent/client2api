package alerts

import (
	"path/filepath"
	"testing"
	"time"
)

func TestClearDropsHistoryAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.json")
	s := NewStore(10, path)
	s.Add(Alert{At: time.Now(), Kind: "platform_unhealthy", Client: "alpha"})
	s.MarkAllRead()
	if got := len(s.List()); got != 1 {
		t.Fatalf("List() = %d, want 1", got)
	}
	if got := s.Unread(); got != 0 {
		t.Fatalf("Unread() = %d, want 0 after MarkAllRead", got)
	}

	s.Clear()
	if got := len(s.List()); got != 0 {
		t.Fatalf("List() = %d after Clear, want 0", got)
	}
	if got := s.Unread(); got != 0 {
		t.Fatalf("Unread() = %d after Clear, want 0", got)
	}

	reloaded := NewStore(10, path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(reloaded.List()); got != 0 {
		t.Fatalf("reloaded List() = %d, want the cleared journal to survive", got)
	}
}

func TestClearOnNilStoreIsSafe(t *testing.T) {
	var s *Store
	s.Clear()
}
