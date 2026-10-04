package alerts

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreKeepsNewestAlertsAndCaps(t *testing.T) {
	s := NewStore(2, "")
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	for i := 0; i < 3; i++ {
		s.Add(Alert{At: base.Add(time.Duration(i) * time.Minute), Kind: "platform_unhealthy", Client: "tabbit", Model: "m"})
	}
	got := s.List()
	if len(got) != 2 {
		t.Fatalf("List returned %d alerts, want 2", len(got))
	}
	if !got[0].At.Equal(base.Add(2*time.Minute)) || !got[1].At.Equal(base.Add(time.Minute)) {
		t.Fatalf("alerts = %+v, want newest first", got)
	}
}

func TestStorePersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.json")
	s := NewStore(10, path)
	want := Alert{At: time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local), Kind: "platform_unhealthy", Client: "tabbit", Model: "deepseek", Account: "a1", Detail: "three failed rounds", Count: 3}
	s.Add(want)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded := NewStore(10, path)
	if err := loaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := loaded.List()
	if len(got) != 1 || got[0].Client != want.Client || got[0].Model != want.Model || got[0].Count != want.Count {
		t.Fatalf("loaded alerts = %+v, want %+v", got, want)
	}
}
