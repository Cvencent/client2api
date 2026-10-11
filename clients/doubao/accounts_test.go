package doubao

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"client2api/internal/core"
)

// newTestClient builds a client with a temp data dir and no network.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	dir := t.TempDir()
	raw, err := New(core.Deps{DataDir: dir, Config: []byte(`{}`)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return raw.(*Client)
}

func TestStoreAddSnapshotAndPersist(t *testing.T) {
	dir := t.TempDir()
	accPath := filepath.Join(dir, accountsFile)
	statePath := filepath.Join(dir, stateFile)
	s, err := loadStore(accPath, statePath)
	if err != nil {
		t.Fatalf("loadStore: %v", err)
	}
	if _, err := s.add(&account{ID: "a1", Label: "one", Cookie: "sessionid=abc; ttwid=xyz", Enabled: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := s.add(&account{ID: "a2", Label: "two", Cookie: "sessionid=def", Enabled: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	snap := s.snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot has %d accounts, want 2", len(snap))
	}

	// A second store over the same files sees the credentials back.
	reloaded, err := loadStore(accPath, statePath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(reloaded.snapshot()); got != 2 {
		t.Fatalf("reloaded %d accounts, want 2", got)
	}
}

func TestStorePickSkipsDisabledAndCooling(t *testing.T) {
	s := &store{}
	_, _ = s.add(&account{ID: "ready", Cookie: "sessionid=r", Enabled: true})
	_, _ = s.add(&account{ID: "off", Cookie: "sessionid=o", Enabled: false})
	_, _ = s.add(&account{ID: "cool", Cookie: "sessionid=c", Enabled: true})

	s.mu.Lock()
	for _, a := range s.accounts {
		switch a.ID {
		case "cool":
			a.State = stateCooling
			a.CooldownUntil = time.Now().Add(time.Hour)
		case "off":
			a.State = stateReady
		case "ready":
			a.State = stateReady
		}
	}
	s.mu.Unlock()

	picked, err := s.pick()
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if picked.ID != "ready" {
		t.Fatalf("picked %q, want ready", picked.ID)
	}

	// Once the only usable account is parked, pick reports not-configured.
	s.mu.Lock()
	for _, a := range s.accounts {
		if a.ID == "ready" {
			a.State = stateInvalid
		}
		if a.ID == "cool" {
			a.CooldownUntil = time.Now().Add(-time.Second)
			a.State = stateCooling
		}
	}
	s.mu.Unlock()
	picked, err = s.pick()
	if err != nil {
		t.Fatalf("pick after cooldown lapsed: %v", err)
	}
	if picked.ID != "cool" {
		t.Fatalf("picked %q, want cool (its cooldown lapsed)", picked.ID)
	}
}

func TestStorePickEmptyIsNotConfigured(t *testing.T) {
	s := &store{}
	_, err := s.pick()
	if err != core.ErrNotConfigured {
		t.Fatalf("pick on empty = %v, want ErrNotConfigured", err)
	}
}

func TestCookieIdentityIsOrderIndependentAndSecretFree(t *testing.T) {
	a := cookieIdentity("sessionid=abc; ttwid=xyz")
	b := cookieIdentity("ttwid=xyz; sessionid=abc")
	if a != b {
		t.Fatalf("identity depends on cookie order: %q vs %q", a, b)
	}
	if len(a) == 0 || a[0:3] != "db-" {
		t.Fatalf("identity %q is not a db- prefixed hash", a)
	}
	if contains(a, "abc") || contains(a, "xyz") {
		t.Fatalf("identity %q leaked cookie material", a)
	}
}

func TestHasSessionCookie(t *testing.T) {
	if !hasSessionCookie("ttwid=1; sessionid=abc") {
		t.Error("expected sessionid to count as a session cookie")
	}
	if hasSessionCookie("ttwid=1; msToken=2") {
		t.Error("a jar without sessionid must not count as signed in")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

var _ = context.Background

