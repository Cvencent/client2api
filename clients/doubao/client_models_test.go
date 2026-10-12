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

// TestRoutesFromMenuConfV2UsesModeOwnedLists pins the v2 shape: when a mode
// owns its model list, the flat list is not merged into it. The desktop client
// only falls back to the flat list for a mode that has no model list of its
// own.
func TestRoutesFromMenuConfV2UsesModeOwnedLists(t *testing.T) {
	var cfg menuConfV2
	if err := json.Unmarshal([]byte(`{
	  "use_mode_model_list": true,
	  "mode_list": {"item_list": [
	    {"mode_id": "1", "model_list": {"item_list": [
	      {"item_id": 0, "model_item_key": "0", "name": "Auto"},
	      {"item_id": 9, "model_item_key": "9", "name": "Turbo"}
	    ]}}
	  ]},
	  "model_list": {"item_list": [
	    {"item_id": 8, "model_item_key": "8", "name": "Flat only"},
	    {"item_id": 9, "model_item_key": "9"}
	  ]}
	}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	routes := routesFromMenuConfV2(cfg)
	if len(routes) != 2 {
		t.Fatalf("got %d routes, want 2 (flat-only models must not be merged into a mode-owned list)", len(routes))
	}
	if routeID(routes[0]) != "mode1_model0" || routeID(routes[1]) != "mode1_model9" {
		t.Fatalf("route ids = %q,%q", routeID(routes[0]), routeID(routes[1]))
	}
}

// TestRoutesFromMenuConfV2FallsBackToFlatForModeWithoutModels pins the one
// fallback in the desktop client's v2 mode lookup: a mode with no model list
// uses the flat list, and every route belongs to that mode.
func TestRoutesFromMenuConfV2FallsBackToFlatForModeWithoutModels(t *testing.T) {
	var cfg menuConfV2
	if err := json.Unmarshal([]byte(`{
	  "use_mode_model_list": true,
	  "mode_list": {"item_list": [
	    {"mode_id": "7", "model_list": {"item_list": []}}
	  ]},
	  "model_list": {"item_list": [
	    {"item_id": 9, "model_item_key": "9", "name": "Turbo"}
	  ]}
	}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	routes := routesFromMenuConfV2(cfg)
	if len(routes) != 1 || routeID(routes[0]) != "mode7_model9" {
		t.Fatalf("routes = %+v, want the flat route assigned to mode 7", routes)
	}
}

// TestRoutesFromMenuConfV2UsesSupportModels pins the flat v2 shape used when
// use_mode_model_list is false: support_models decides which modes can route a
// model, so the same model remains selectable under each supporting mode.
func TestRoutesFromMenuConfV2UsesSupportModels(t *testing.T) {
	var cfg menuConfV2
	if err := json.Unmarshal([]byte(`{
	  "use_mode_model_list": false,
	  "mode_list": {"item_list": [
	    {"mode_id": "1", "support_models": [
	      {"item_id": 0, "model_item_key": "0"},
	      {"item_id": 9, "model_item_key": "9"}
	    ]},
	    {"mode_id": "2", "support_models": [
	      {"item_id": 9, "model_item_key": "9"}
	    ]}
	  ]},
	  "model_list": {"item_list": [
	    {"item_id": 0, "model_item_key": "0", "name": "Auto"},
	    {"item_id": 9, "model_item_key": "9", "name": "Turbo"}
	  ]}
	}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	routes := routesFromMenuConfV2(cfg)
	got := map[string]bool{}
	for _, route := range routes {
		got[routeID(route)] = true
	}
	for _, want := range []string{"mode1_model0", "mode1_model9", "mode2_model9"} {
		if !got[want] {
			t.Fatalf("routes = %+v, missing %s", routes, want)
		}
	}
	if len(routes) != len(got) {
		t.Fatalf("routes contain duplicate ids: %+v", routes)
	}
}

// TestRouteFromItemAppliesLegacyMode2Model4Rule pins the vendor's routing
// compatibility rule: the visible mode/model pair stays 2/4, while reply_mode
// is sent as 3.
func TestRouteFromItemAppliesLegacyMode2Model4Rule(t *testing.T) {
	route, ok := routeFromItem(menuItem{ModeID: "2", ModelItemKey: "4", Name: "Special"}, v2ModelKey)
	if !ok {
		t.Fatal("routeFromItem rejected a valid v2 route")
	}
	if route.ModeID != "2" || route.ModelItemKey != "4" || route.ReplyMode != "3" {
		t.Fatalf("route = %+v, want mode 2, model 4, reply_mode 3", route)
	}
}

// TestRoutesFromMenuConfV2UsesItemIDNotMenuType pins the v2 model key: the
// desktop client's v2 selection code resolves items by model_item_key then
// item_id and never reads menu_type, unlike the legacy v1 menu. An item that
// carries both menu_type and item_id must therefore route as its item_id.
func TestRoutesFromMenuConfV2UsesItemIDNotMenuType(t *testing.T) {
	var cfg menuConfV2
	if err := json.Unmarshal([]byte(`{
	  "use_mode_model_list": false,
	  "mode_list": {"item_list": [
	    {"mode_id": "3", "support_models": [{"item_id": 9, "menu_type": 1}]}
	  ]},
	  "model_list": {"item_list": [
	    {"item_id": 9, "menu_type": 1, "name": "V2 Turbo"}
	  ]}
	}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	routes := routesFromMenuConfV2(cfg)
	if len(routes) != 1 || routeID(routes[0]) != "mode3_model9" {
		t.Fatalf("routes = %+v, want mode3_model9 (v2 key is item_id, not menu_type)", routes)
	}
}

// TestParseClientMenuV1UsesMenuTypeNotItemID pins the v1 model key: the legacy
// menu resolves an item by model_item_key then menu_type then item_id, so a
// v1 item with both fields set routes as its menu_type.
func TestParseClientMenuV1UsesMenuTypeNotItemID(t *testing.T) {
	raw := json.RawMessage(`{
	  "menu_item_list": [
	    {"item_id": 9, "menu_type": 1, "name": "Legacy"}
	  ]
	}`)
	routes, err := parseClientMenu(raw)
	if err != nil {
		t.Fatalf("parseClientMenu: %v", err)
	}
	if len(routes) != 1 || routes[0].ModelItemKey != "1" {
		t.Fatalf("routes = %+v, want model_item_key 1 (v1 key is menu_type, not item_id)", routes)
	}
}

// TestPullAccountMenuUsesV2WithoutReadingV1 pins the v2 switch: once the
// desktop client is serving v2, the old v1 catalogue is not a fallback or an
// additional source.
func TestPullAccountMenuUsesV2WithoutReadingV1(t *testing.T) {
	var v1Hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathActionBarBriefList:
			_, _ = w.Write([]byte(`{"data":{"entry_list":[
			  {"action_bar_key":"coco_deep_thinking","active_switch_conf":{
			    "use_menu_conf_v2":true,
			    "menu_conf_v2":{"model_list":{"item_list":[
			      {"item_id":9,"model_item_key":"9","mode_id":"3","name":"Turbo","reasoning_effort_config":{"default_level":"high"}}]}}}}]}}`))
		case pathUserSettingGet:
			v1Hits++
			_, _ = w.Write([]byte(`{"data":{"action_bar_menu_config":{"menu_item_list":[
			  {"item_id":9,"menu_type":9,"name":"duplicate"},
			  {"item_id":0,"menu_type":0,"name":"快速回答"},
			  {"item_id":1,"menu_type":1,"name":"深度思考"}]}}}`))
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
		t.Fatalf("v1 endpoint was read %d times with v2 enabled, want 0", v1Hits)
	}
	if _, ok := menu.resolveRoute("mode3_model9"); !ok {
		t.Fatalf("v2 route not resolvable; routes=%+v", menu.routes)
	}
	if len(menu.routes) != 1 {
		t.Fatalf("routes = %+v, want only the v2 route", menu.routes)
	}
	if routeID(menu.routes[0]) != "mode3_model9" || menu.routes[0].ReasoningEffort != "high" {
		t.Fatalf("route = %+v, want the v2 metadata", menu.routes[0])
	}
}

// TestPullAccountMenuRejectsEmptyV2 pins the failure contract: an enabled but
// empty v2 menu is an error, not permission to silently read v1.
func TestPullAccountMenuRejectsEmptyV2(t *testing.T) {
	var v1Hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathActionBarBriefList:
			_, _ = w.Write([]byte(`{"data":{"entry_list":[{"active_switch_conf":{"use_menu_conf_v2":true,"menu_conf_v2":{}}}]}}`))
		case pathUserSettingGet:
			v1Hits++
			_, _ = w.Write([]byte(`{"data":{"action_bar_menu_config":{"menu_item_list":[{"item_id":0,"menu_type":0}]}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newWebClient(t, srv.URL)
	acc := &account{ID: "a1", Cookie: "sessionid=abc", Enabled: true}
	if _, err := c.pullAccountMenu(context.Background(), acc); err == nil {
		t.Fatal("pullAccountMenu accepted an enabled but empty v2 menu")
	}
	if v1Hits != 0 {
		t.Fatalf("v1 endpoint was read %d times after an empty v2 menu, want 0", v1Hits)
	}
}

// TestPullAccountMenuUsesV1WhenV2IsOff pins the legacy branch.
func TestPullAccountMenuUsesV1WhenV2IsOff(t *testing.T) {
	var v1Hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathActionBarBriefList:
			_, _ = w.Write([]byte(`{"data":{"entry_list":[{"active_switch_conf":{"use_menu_conf_v2":false}}]}}`))
		case pathUserSettingGet:
			v1Hits++
			_, _ = w.Write([]byte(`{"data":{"action_bar_menu_config":{"menu_item_list":[{"item_id":9,"menu_type":9,"name":"Legacy"}]}}}`))
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
	if v1Hits != 1 {
		t.Fatalf("v1 endpoint was read %d times, want 1", v1Hits)
	}
	if len(menu.routes) != 1 || routeID(menu.routes[0]) != "mode3_model9" {
		t.Fatalf("routes = %+v, want the v1 route", menu.routes)
	}
}

// TestRefreshModelsKeepsSameModelUnderDifferentModes pins the catalogue id
// contract: the routing id includes the mode, so merging accounts must not
// collapse two selectable modes that happen to share a model item key.
func TestRefreshModelsKeepsSameModelUnderDifferentModes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathActionBarBriefList {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"entry_list":[{"active_switch_conf":{"use_menu_conf_v2":true,"menu_conf_v2":{"use_mode_model_list":true,"mode_list":{"item_list":[
		  {"mode_id":"1","model_list":{"item_list":[{"item_id":9,"model_item_key":"9","name":"Mode 1"}]}},
		  {"mode_id":"2","model_list":{"item_list":[{"item_id":9,"model_item_key":"9","name":"Mode 2"}]}}
		]}}}}]}}`))
	}))
	defer srv.Close()

	c := newWebClient(t, srv.URL)
	_, _ = c.store.add(&account{ID: "a1", Cookie: "sessionid=abc", Enabled: true})
	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	got := map[string]bool{}
	for _, model := range models {
		got[model.ID] = true
	}
	for _, want := range []string{"mode1_model9", "mode2_model9"} {
		if !got[want] {
			t.Fatalf("catalogue = %+v, missing %s", models, want)
		}
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
