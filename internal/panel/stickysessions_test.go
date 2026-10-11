package panel

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/gateway"
)

type fakePlatformStore struct {
	mu     sync.Mutex
	routes map[string]gateway.StickyRoute
}

func (f *fakePlatformStore) List() []gateway.StickyRoute {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]gateway.StickyRoute, 0, len(f.routes))
	for _, row := range f.routes {
		out = append(out, row)
	}
	return out
}

func (f *fakePlatformStore) Forget(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.routes[key]; !ok {
		return false
	}
	delete(f.routes, key)
	return true
}

func stickyPanel(t *testing.T, platforms SessionPlatformStore, clients ...core.Client) http.Handler {
	t.Helper()
	return New(Options{
		Registry:         registryOf(clients...),
		Version:          "test",
		Listen:           "127.0.0.1:0",
		Started:          time.Now(),
		SessionPlatforms: platforms,
	})
}

// TestStickySessionsListMergesAccountAndPlatformBindings pins the read model
// the homepage dialog renders: one row per account binding, with the shared
// platform route attached, plus platform-only rows for clients without a local
// account table.
func TestStickySessionsListMergesAccountAndPlatformBindings(t *testing.T) {
	c := binderClient("verbs", core.AccountRecord{ID: "A1"})
	c.entries["conv-1"] = "A1"
	platforms := &fakePlatformStore{routes: map[string]gateway.StickyRoute{
		"conv-1": {Key: "conv-1", Platform: "verbs"},
		"conv-2": {Key: "conv-2", Platform: "other"},
	}}
	h := stickyPanel(t, platforms, c)

	rec := hitRoute(t, h, http.MethodGet, "/panel/api/sticky-sessions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)
	rows, ok := body["sessions"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("sessions = %#v, want two rows", body["sessions"])
	}
	first := rows[0].(map[string]any)
	if first["key"] != "conv-1" || first["client"] != "verbs" || first["account"] != "A1" || first["platform"] != "verbs" {
		t.Fatalf("merged row = %#v", first)
	}
	if first["account_bound"] != true || first["platform_bound"] != true {
		t.Fatalf("merged row did not report both bindings: %#v", first)
	}
	second := rows[1].(map[string]any)
	if second["key"] != "conv-2" || second["client"] != nil || second["platform"] != "other" {
		t.Fatalf("platform-only row = %#v", second)
	}
}

// TestStickySessionUnbindTouchesOnlyTheAccountLayer pins the operator's chosen
// semantics: 解绑 must leave the gateway's platform route in place.
func TestStickySessionUnbindTouchesOnlyTheAccountLayer(t *testing.T) {
	c := binderClient("verbs", core.AccountRecord{ID: "A1"})
	c.entries["conv-1"] = "A1"
	platforms := &fakePlatformStore{routes: map[string]gateway.StickyRoute{
		"conv-1": {Key: "conv-1", Platform: "verbs"},
	}}
	h := stickyPanel(t, platforms, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/sticky-sessions/unbind", `{"client":"verbs","key":"conv-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := c.bound("conv-1"); ok {
		t.Fatal("the account binding survived 解绑")
	}
	if len(platforms.routes) != 1 {
		t.Fatal("解绑 cleared the platform route too")
	}
}

// TestStickySessionSwitchClearsBothLayers pins the more forceful action: the
// next request must re-rank platforms and accounts from a clean slate.
func TestStickySessionSwitchClearsBothLayers(t *testing.T) {
	c := binderClient("verbs", core.AccountRecord{ID: "A1"})
	c.entries["conv-1"] = "A1"
	platforms := &fakePlatformStore{routes: map[string]gateway.StickyRoute{
		"conv-1": {Key: "conv-1", Platform: "verbs"},
	}}
	h := stickyPanel(t, platforms, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/sticky-sessions/switch", `{"client":"verbs","key":"conv-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeMap(t, rec)
	if got["forgotten_account"] != true || got["forgotten_platform"] != true {
		t.Fatalf("switch response = %s", rec.Body.String())
	}
	if _, ok := c.bound("conv-1"); ok {
		t.Fatal("the account binding survived 强制切换")
	}
	if len(platforms.routes) != 0 {
		t.Fatal("the platform route survived 强制切换")
	}
}
