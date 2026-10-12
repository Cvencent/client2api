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

func (f *fakePlatformStore) Pin(key, platform, model string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key == "" || platform == "" {
		return false
	}
	f.routes[key] = gateway.StickyRoute{Key: key, Platform: platform, Model: model}
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
		"conv-1": {Key: "conv-1", Platform: "verbs", Model: "m1"},
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
	if first["model"] != "m1" {
		t.Fatalf("merged row model = %v, want m1", first["model"])
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

// TestStickySessionSwitchPinsAChosenPlatform pins the operator's targeted
// switch: the account binding is cleared, and the gateway route is rewritten to
// the chosen platform so the next turn starts there.
func TestStickySessionSwitchPinsAChosenPlatform(t *testing.T) {
	beta := binderClient("beta", core.AccountRecord{ID: "B1"})
	beta.entries["conv-1"] = "B1"
	beta.models = []core.Model{{ID: "m1", OwnedBy: "beta"}}
	alpha := &fakeClient{name: "alpha", models: []core.Model{{ID: "m1", OwnedBy: "alpha"}}}
	platforms := &fakePlatformStore{routes: map[string]gateway.StickyRoute{
		"conv-1": {Key: "conv-1", Platform: "beta", Model: "m1"},
	}}
	h := stickyPanel(t, platforms, alpha, beta)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/sticky-sessions/switch", `{"client":"beta","key":"conv-1","platform":"alpha","model":"m1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeMap(t, rec)
	if got["forgotten_account"] != true || got["platform"] != "alpha" {
		t.Fatalf("switch response = %s", rec.Body.String())
	}
	if _, ok := beta.bound("conv-1"); ok {
		t.Fatal("the account binding survived a targeted switch")
	}
	route := platforms.routes["conv-1"]
	if route.Platform != "alpha" || route.Model != "m1" {
		t.Fatalf("route = %#v, want alpha carrying m1", route)
	}
}

// TestStickySessionSwitchValidatesTheChosenPlatformBeforeMutating keeps a bad
// target from erasing the current working binding.
func TestStickySessionSwitchValidatesTheChosenPlatformBeforeMutating(t *testing.T) {
	beta := binderClient("beta", core.AccountRecord{ID: "B1"})
	beta.entries["conv-1"] = "B1"
	beta.models = []core.Model{{ID: "m1", OwnedBy: "beta"}}
	alpha := &fakeClient{name: "alpha", models: []core.Model{{ID: "m2", OwnedBy: "alpha"}}}
	platforms := &fakePlatformStore{routes: map[string]gateway.StickyRoute{
		"conv-1": {Key: "conv-1", Platform: "beta", Model: "m1"},
	}}
	h := stickyPanel(t, platforms, alpha, beta)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/sticky-sessions/switch", `{"client":"beta","key":"conv-1","platform":"alpha","model":"m1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := beta.bound("conv-1"); !ok {
		t.Fatal("failed validation cleared the account binding")
	}
	if route := platforms.routes["conv-1"]; route.Platform != "beta" {
		t.Fatalf("failed validation changed the route: %#v", route)
	}
}
