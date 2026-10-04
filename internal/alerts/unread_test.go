package alerts

import (
	"path/filepath"
	"testing"
	"time"
)

// TestStoreTracksUnreadAlerts pins the distinction the badge needs: total
// history vs. alerts the operator has not seen yet.
func TestStoreTracksUnreadAlerts(t *testing.T) {
	s := NewStore(10, "")
	s.Add(Alert{At: time.Unix(1, 0), Kind: "a"})
	s.Add(Alert{At: time.Unix(2, 0), Kind: "b"})
	if got := s.Unread(); got != 2 {
		t.Fatalf("unread = %d, want 2", got)
	}
	s.MarkAllRead()
	if got := s.Unread(); got != 0 {
		t.Fatalf("unread after MarkAllRead = %d, want 0", got)
	}
	if got := len(s.List()); got != 2 {
		t.Fatalf("history length = %d, want the two alerts kept", got)
	}
}

// TestStorePersistsReadState pins that a restart does not resurrect a badge
// the operator already cleared.
func TestStorePersistsReadState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.json")
	s := NewStore(10, path)
	s.Add(Alert{At: time.Unix(1, 0), Kind: "a"})
	s.MarkAllRead()
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	again := NewStore(10, path)
	if err := again.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := again.Unread(); got != 0 {
		t.Fatalf("unread after reload = %d, want 0", got)
	}
}

// TestStoreNewAlertAfterReadIsUnread is the other half: reading old alerts must
// not suppress a new one.
func TestStoreNewAlertAfterReadIsUnread(t *testing.T) {
	s := NewStore(10, "")
	s.Add(Alert{At: time.Unix(1, 0), Kind: "a"})
	s.MarkAllRead()
	s.Add(Alert{At: time.Unix(2, 0), Kind: "b"})
	if got := s.Unread(); got != 1 {
		t.Fatalf("unread = %d, want the one alert added after the read mark", got)
	}
}
