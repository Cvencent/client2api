package doubao

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"client2api/internal/core"
)

// TestParseClientMenuV1 pins the v1 shape: menu_item_list entries are modes
// that carry only menu_type, so they inherit the module's default mode and use
// their own menu_type as the model item key.
func TestParseClientMenuV1(t *testing.T) {
	raw := json.RawMessage(`{
	  "menu_item_list": [
	    {"item_id": 0, "menu_type": 0, "name": "快速回答", "agent_mode": 2},
	    {"item_id": 1, "menu_type": 1, "name": "深度思考", "agent_mode": 2}
	  ]
	}`)
	routes, err := parseClientMenu(raw)
	if err != nil {
		t.Fatalf("parseClientMenu: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("got %d routes, want 2", len(routes))
	}
	for _, r := range routes {
		if r.ModeID != defaultModeID {
			t.Errorf("mode_id = %q, want %q", r.ModeID, defaultModeID)
		}
	}
	if routes[0].ModelItemKey != "0" || routes[1].ModelItemKey != "1" {
		t.Fatalf("model item keys = %q,%q", routes[0].ModelItemKey, routes[1].ModelItemKey)
	}
	if routes[1].DisplayName != "深度思考" {
		t.Fatalf("display name = %q", routes[1].DisplayName)
	}
}

// TestRoutesFromMenuConfV2 pins the v2 shape, where each mode owns its model
// list and the mode id is the routing mode.
func TestRoutesFromMenuConfV2(t *testing.T) {
	var cfg menuConfV2
	if err := json.Unmarshal([]byte(`{
	  "use_mode_model_list": true,
	  "mode_list": {"item_list": [
	    {"mode_id": "1", "model_list": {"item_list": [
	      {"item_id": 0, "model_item_key": "0", "name": "Auto"},
	      {"item_id": 9, "model_item_key": "9", "name": "Turbo"}
	    ]}}
	  ]},
	  "model_list": {"item_list": [{"item_id": 9, "model_item_key": "9"}]}
	}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	routes := routesFromMenuConfV2(cfg)
	if len(routes) != 2 {
		t.Fatalf("got %d routes, want 2 (flat list must not duplicate the mode-owned one)", len(routes))
	}
	if routeID(routes[0]) != "mode1_model0" || routeID(routes[1]) != "mode1_model9" {
		t.Fatalf("route ids = %q,%q", routeID(routes[0]), routeID(routes[1]))
	}
}

// TestPullAccountMenuPrefersV2ThenFallsBackToV1 pins the endpoint order: the
// signed-in account gets its v2 menu when the server offers one, and the v1
// menu is only read when the v2 menu is absent.
func TestPullAccountMenuPrefersV2ThenFallsBackToV1(t *testing.T) {
	var v1Hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathActionBarBriefList:
			_, _ = w.Write([]byte(`{"data":{"entry_list":[
			  {"action_bar_key":"coco_deep_thinking","active_switch_conf":{
			    "use_menu_conf_v2":true,
			    "menu_conf_v2":{"model_list":{"item_list":[
			      {"item_id":9,"model_item_key":"9","mode_id":"3","name":"Turbo"}]}}}}]}}`))
		case pathUserSettingGet:
			v1Hits++
			_, _ = w.Write([]byte(`{"data":{"action_bar_menu_config":{"menu_item_list":[]}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newWebClient(t, srv.URL)
	acc := &account{ID: "a1", Cookie: "sessionid=abc", Enabled: true}
	menu, err := c.pullAccountMenu(context.Background(), acc)
	if err != nil {
		t.Fatalf("pullAccountMenu: %v", err)
	}
	if v1Hits != 0 {
		t.Fatalf("v1 endpoint was read %d times despite a v2 menu", v1Hits)
	}
	if _, ok := menu.resolveRoute("mode3_model9"); !ok {
		t.Fatalf("v2 route not resolvable; routes=%+v", menu.routes)
	}
}

// TestRefreshModelsFallsBackToDefault pins the cold-start catalogue: with no
// readable menu the module still advertises its default routing id.
func TestRefreshModelsFallsBackToDefault(t *testing.T) {
	c := newWebClient(t, "http://127.0.0.1:1")
	models, _ := c.RefreshModels(context.Background())
	if len(models) == 0 {
		t.Fatalf("catalogue is empty")
	}
	found := false
	for _, m := range models {
		if m.ID == "default" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fallback catalogue = %+v, want a default entry", models)
	}
}

// TestRouteForFallsBackToClientDefault pins that an unadvertised model still
// routes with the client's own verified default pair.
func TestRouteForFallsBackToClientDefault(t *testing.T) {
	c := newWebClient(t, "http://127.0.0.1:1")
	acc := &account{ID: "a1"}
	r := c.routeFor(acc, "no-such-model")
	if r.ModeID != defaultModeID || r.ModelItemKey != defaultModelItemKey {
		t.Fatalf("route = %+v, want mode %s model %s", r, defaultModeID, defaultModelItemKey)
	}
}

var _ = core.Model{}
