package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// client_models.go reads the model menu the Doubao desktop client itself is
// showing.  The desktop client is a Chromium shell around www.doubao.com, so
// "the models in the client" are the entries the server returns in
// action_bar_menu_config (the same payload the client renders in its model
// picker).  There is no separate local model API.
//
// Each entry carries the routing triple the client puts on a message's `ext`
// when the user picks that model:
//
//	reply_mode, mode_id, model_item_key
//
// and optionally reasoning_effort.  The gateway stores that triple per account
// (accounts can see different menus) and injects it into the outgoing
// /samantha/chat/completion request so a gateway caller can select any model
// the desktop client can select.

// menuItem is one entry of action_bar_menu_config.menu_item_list or of a
// menuConfV2 model_list.item_list.  Only the routing fields are kept.
type menuItem struct {
	ItemID              json.RawMessage  `json:"item_id"`
	MenuType            json.RawMessage  `json:"menu_type"`
	ModeID              string           `json:"mode_id"`
	ModelItemKey        string           `json:"model_item_key"`
	Name                string           `json:"name"`
	SelectedName        string           `json:"selected_name"`
	SubTitleName        string           `json:"sub_title_name"`
	AgentMode           json.RawMessage  `json:"agent_mode"`
	DefaultMode         string           `json:"default_mode"`
	NeedUpgrade         bool             `json:"need_upgrade"`
	NeedLogin           bool             `json:"need_login"`
	ReasoningEffortConf *reasoningEffort `json:"reasoning_effort_config"`
	SubscribeConfig     *subscribeConfig `json:"subscribe_config"`
	ModelExtraParams    json.RawMessage  `json:"model_extra_params"`
}

type reasoningEffort struct {
	DefaultLevel json.RawMessage `json:"default_level"`
	OptionList   []struct {
		Level json.RawMessage `json:"level"`
	} `json:"option_list"`
}

type subscribeConfig struct {
	NeedUpgrade bool `json:"need_upgrade"`
}

// modelRoute is the metadata the gateway needs to route one model.
type modelRoute struct {
	Key              string
	ModeID           string
	ModelItemKey     string
	ReplyMode        string
	ReasoningEffort  string
	AgentMode        string
	UseDeepThink     string
	DisplayName      string
	ContextWindow    int
	NeedUpgrade      bool
	SupportsImages   bool
	SupportsThinking bool
}

// menuPayload mirrors the parts of action_bar_menu_config (and the v2 menu
// config) we parse.  The v2 config nests model menus under mode_list /
// model_list.
type menuPayload struct {
	MenuItemList []menuItem `json:"menu_item_list"`
	ModeList     struct {
		ItemList []menuItem `json:"item_list"`
	} `json:"mode_list"`
	ModelList struct {
		ItemList []menuItem `json:"item_list"`
	} `json:"model_list"`
	// UseModeModelList marks the v2 shape where each mode owns its own model
	// list; the client then reads model_list out of the matching mode item.
	UseModeModelList bool `json:"use_mode_model_list"`
}

// menuConfV2 is the v2 menu the desktop client prefers: a mode list where each
// mode carries its own model list.  The client reads it from the
// action_bar_v3/brief_list entry's active_switch_conf.menu_conf_v2.
type menuConfV2 struct {
	UseModeModelList bool `json:"use_mode_model_list"`
	ModeList         struct {
		ItemList []menuModeItem `json:"item_list"`
	} `json:"mode_list"`
	ModelList struct {
		ItemList []menuItem `json:"item_list"`
	} `json:"model_list"`
}

// menuModeItem is one mode in the v2 mode list.  A mode owns its own model
// list when use_mode_model_list is set.
type menuModeItem struct {
	ModeID        string          `json:"mode_id"`
	AgentMode     json.RawMessage `json:"agent_mode"`
	Name          string          `json:"name"`
	SelectedName  string          `json:"selected_name"`
	DefaultMode   bool            `json:"default_mode"`
	SupportModels []menuItem      `json:"support_models"`
	ModelList     struct {
		ItemList []menuItem `json:"item_list"`
	} `json:"model_list"`
}

// actionBarEntry is one entry of the v2 action-bar brief list.
type actionBarEntry struct {
	ActionBarKey     string `json:"action_bar_key"`
	ActiveSwitchConf struct {
		UseMenuConfV2 bool       `json:"use_menu_conf_v2"`
		MenuConfV2    menuConfV2 `json:"menu_conf_v2"`
	} `json:"active_switch_conf"`
}

// actionBarBrief is the brief_list reply envelope.
type actionBarBrief struct {
	EntryList []actionBarEntry `json:"entry_list"`
}

// clientMenu is one account's parsed catalogue.
type clientMenu struct {
	routes    []modelRoute
	byKey     map[string]modelRoute
	fetchedAt time.Time
}

// defaultModeID and defaultModelItemKey are the routing fields the client's own
// router uses when a menu entry does not carry them: mode 3 with model item "0",
// read out of a signed-in session's conversation extra.
const (
	defaultModeID       = "3"
	defaultModelItemKey = "0"
)

// menuFetchTimeout bounds one model-menu pull.
var menuFetchTimeout = 25 * time.Second

// accountMenu returns the cached menu for one account, or false.
func (c *Client) accountMenu(accountID string) (clientMenu, bool) {
	c.menuMu.Lock()
	defer c.menuMu.Unlock()
	m, ok := c.menus[accountID]
	return m, ok
}

// setAccountMenu caches one account's menu.
func (c *Client) setAccountMenu(accountID string, m clientMenu) {
	c.menuMu.Lock()
	defer c.menuMu.Unlock()
	if c.menus == nil {
		c.menus = map[string]clientMenu{}
	}
	c.menus[accountID] = m
}

// pullAccountMenu asks the account for the client's model menu and parses it.
// The current client prefers the v2 menu on action_bar_v3/brief_list. When that
// endpoint explicitly enables use_menu_conf_v2, it is the sole menu source:
// the client does not merge the legacy action_bar_menu_config. Only an account
// that has not rolled into v2 uses the v1 menu on user/setting/get.
func (c *Client) pullAccountMenu(ctx context.Context, acc *account) (clientMenu, error) {
	ctx, cancel := context.WithTimeout(ctx, menuFetchTimeout)
	defer cancel()

	v2, useV2 := c.pullV2Menu(ctx, acc)
	if useV2 {
		if len(v2) == 0 {
			return clientMenu{}, fmt.Errorf("doubao: v2 is enabled but the account returned an empty client model menu")
		}
		return newClientMenu(v2), nil
	}
	v1, err := c.pullV1Menu(ctx, acc)
	if err != nil {
		return clientMenu{}, err
	}
	if len(v1) == 0 {
		return clientMenu{}, fmt.Errorf("doubao: the account returned no client model menu")
	}
	return newClientMenu(v1), nil
}

// pullV2Menu reads action_bar_v3/brief_list and extracts the v2 menu when the
// account has one. A missing or switched-off menu is not an error: enabled is
// false so the caller falls back to v1. An enabled menu is authoritative even
// when it parses to no routes.
func (c *Client) pullV2Menu(ctx context.Context, acc *account) ([]modelRoute, bool) {
	var out struct {
		Data actionBarBrief `json:"data"`
	}
	if err := c.up.postJSON(ctx, acc, pathActionBarBriefList, map[string]any{
		"language_code": "zh",
		"bot_id":        c.chatBotID(),
	}, &out); err != nil {
		return nil, false
	}
	for _, entry := range out.Data.EntryList {
		if !entry.ActiveSwitchConf.UseMenuConfV2 {
			continue
		}
		return routesFromMenuConfV2(entry.ActiveSwitchConf.MenuConfV2), true
	}
	return nil, false
}

// pullV1Menu reads the v1 action_bar_menu_config.
func (c *Client) pullV1Menu(ctx context.Context, acc *account) ([]modelRoute, error) {
	var out struct {
		Data struct {
			ActionBarMenuConfig json.RawMessage `json:"action_bar_menu_config"`
		} `json:"data"`
	}
	if err := c.up.postJSON(ctx, acc, pathUserSettingGet, map[string]any{
		"ab_param": map[string]any{"ab_request": map[string]any{}},
	}, &out); err != nil {
		return nil, err
	}
	return parseClientMenu(out.Data.ActionBarMenuConfig)
}

// newClientMenu indexes a route list.
func newClientMenu(routes []modelRoute) clientMenu {
	return clientMenu{routes: routes, byKey: indexRoutes(routes), fetchedAt: time.Now()}
}

// routesFromMenuConfV2 flattens a v2 menu using the same branches as the
// desktop client. When use_mode_model_list is true each mode owns its model
// list, with the flat list used only for a mode that has no list of its own.
// Otherwise the flat list is the candidate set and support_models assigns each
// model to every mode that can route it.
func routesFromMenuConfV2(cfg menuConfV2) []modelRoute {
	var routes []modelRoute
	seen := map[string]bool{}
	addItems := func(modeID string, items []menuItem) {
		for _, item := range items {
			if modeID != "" {
				item.ModeID = modeID
			}
			route, ok := routeFromItem(item, v2ModelKey)
			if !ok {
				continue
			}
			key := strings.ToLower(routeID(route))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			routes = append(routes, route)
		}
	}
	if cfg.UseModeModelList {
		if len(cfg.ModeList.ItemList) == 0 {
			addItems("", cfg.ModelList.ItemList)
			return routes
		}
		for _, mode := range cfg.ModeList.ItemList {
			items := mode.ModelList.ItemList
			if len(items) == 0 {
				items = cfg.ModelList.ItemList
			}
			addItems(mode.ModeID, items)
		}
		return routes
	}

	if len(cfg.ModeList.ItemList) == 0 {
		addItems("", cfg.ModelList.ItemList)
		return routes
	}
	for _, item := range cfg.ModelList.ItemList {
		assigned := false
		modelKey := v2ModelKey(item)
		for _, mode := range cfg.ModeList.ItemList {
			if !modeSupportsModel(mode, modelKey) {
				continue
			}
			assigned = true
			addItems(mode.ModeID, []menuItem{item})
		}
		if !assigned {
			addItems(firstNonEmpty(item.ModeID, item.DefaultMode), []menuItem{item})
		}
	}
	return routes
}

// v2ModelKey mirrors the desktop client's v2 model lookup key: model_item_key
// then item_id. The v2 selection code never consults menu_type, so an item that
// carries both menu_type and item_id routes as its item_id.
func v2ModelKey(item menuItem) string {
	if key := strings.TrimSpace(item.ModelItemKey); key != "" {
		return key
	}
	return rawScalar(item.ItemID)
}

// v1ModelKey mirrors the desktop client's legacy model lookup key:
// model_item_key then menu_type then item_id.
func v1ModelKey(item menuItem) string {
	if key := strings.TrimSpace(item.ModelItemKey); key != "" {
		return key
	}
	return firstNonEmpty(rawScalar(item.MenuType), rawScalar(item.ItemID))
}

func modeSupportsModel(mode menuModeItem, modelKey string) bool {
	if modelKey == "" {
		return false
	}
	for _, supported := range mode.SupportModels {
		if v2ModelKey(supported) == modelKey {
			return true
		}
	}
	return false
}

// parseClientMenu turns the vendor menu JSON into routing entries.  It accepts
// both the v1 (menu_item_list) and v2 (mode_list/model_list) shapes so a
// vendor rollout in either direction keeps working.
func parseClientMenu(raw json.RawMessage) ([]modelRoute, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	// Some payloads wrap the menu as a JSON string.
	if raw[0] == '"' {
		var inner string
		if err := json.Unmarshal(raw, &inner); err == nil && strings.HasPrefix(strings.TrimSpace(inner), "{") {
			raw = json.RawMessage(inner)
		}
	}

	var payload menuPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("doubao: parsing the client model menu: %w", err)
	}

	// v1: menu_item_list entries are modes (only menu_type is set).  v2 flat:
	// model_list entries carry the mode/model pair.
	candidates := make([]menuItem, 0, len(payload.MenuItemList)+len(payload.ModelList.ItemList))
	candidates = append(candidates, payload.MenuItemList...)
	candidates = append(candidates, payload.ModelList.ItemList...)

	routes := make([]modelRoute, 0, len(candidates))
	seen := map[string]bool{}
	for _, item := range candidates {
		route, ok := routeFromItem(item, v1ModelKey)
		if !ok || seen[route.Key] {
			continue
		}
		seen[route.Key] = true
		routes = append(routes, route)
	}
	return routes, nil
}

// routeFromItem builds one routing entry.  A v2 menu item carries mode_id and
// model_item_key outright.  A v1 menu item is a *mode* -- it carries only
// menu_type -- so its mode id is the module default and its model item key is
// that menu_type, which is what the client's own router falls back to. The key
// function selects the v1 or v2 lookup rule.
func routeFromItem(item menuItem, key func(menuItem) string) (modelRoute, bool) {
	model := key(item)
	mode := strings.TrimSpace(item.ModeID)
	if model == "" && mode == "" {
		return modelRoute{}, false
	}
	if strings.HasPrefix(model, "action_bar") || strings.HasPrefix(mode, "action_bar") {
		return modelRoute{}, false
	}

	route := modelRoute{
		Key:          firstNonEmpty(model, mode),
		ModeID:       mode,
		ModelItemKey: model,
		DisplayName:  firstNonEmpty(item.SelectedName, item.Name, item.SubTitleName),
		NeedUpgrade:  item.NeedUpgrade || item.NeedLogin || (item.SubscribeConfig != nil && item.SubscribeConfig.NeedUpgrade),
	}
	// The client always sends all three routing fields; fill the mode default
	// when the menu entry itself did not carry one (the v1 shape).
	if route.ModeID == "" {
		route.ModeID = defaultModeID
	}
	if route.ModelItemKey == "" {
		route.ModelItemKey = defaultModelItemKey
	}
	route.ReplyMode = legacyReplyMode(route.ModeID, route.ModelItemKey)
	if item.ReasoningEffortConf != nil {
		route.ReasoningEffort = rawScalar(item.ReasoningEffortConf.DefaultLevel)
	}
	route.AgentMode = rawScalar(item.AgentMode)
	route.ContextWindow = contextWindow(item.ModelExtraParams)
	route.SupportsThinking = item.ReasoningEffortConf != nil ||
		rawScalar(item.AgentMode) != "" || rawScalar(item.MenuType) != ""
	return route, true
}

// legacyReplyMode is the client's compatibility mapping for the old
// modeSelectValue field: mode 2 / model 4 is sent as reply_mode 3.
func legacyReplyMode(mode, model string) string {
	if mode == "2" && model == "4" {
		return "3"
	}
	return firstNonEmpty(model, mode)
}

func indexRoutes(routes []modelRoute) map[string]modelRoute {
	out := make(map[string]modelRoute, len(routes))
	for _, r := range routes {
		out[strings.ToLower(routeID(r))] = r
		if k := strings.ToLower(strings.TrimSpace(r.ModelItemKey)); k != "" {
			if _, ok := out[k]; !ok {
				out[k] = r
			}
		}
	}
	return out
}

// routeID is the public model id the gateway advertises for one routing entry.
// It folds the mode and the model item together so two modes that share a model
// item key ("快速回答" and "深度思考" both sit under mode 3) stay distinct.
func routeID(r modelRoute) string {
	mode := strings.TrimSpace(r.ModeID)
	item := strings.TrimSpace(r.ModelItemKey)
	switch {
	case mode != "" && item != "":
		return "mode" + mode + "_model" + item
	case mode != "":
		return "mode" + mode
	default:
		return r.Key
	}
}

// resolveRoute finds the route for a requested model id: the advertised id
// first, then the model item key, then a loose match on the display name.
func (m clientMenu) resolveRoute(model string) (modelRoute, bool) {
	want := strings.TrimSpace(model)
	if want == "" {
		return modelRoute{}, false
	}
	if r, ok := m.byKey[strings.ToLower(want)]; ok {
		return r, true
	}
	for _, r := range m.routes {
		if strings.EqualFold(routeID(r), want) || strings.EqualFold(r.DisplayName, want) {
			return r, true
		}
	}
	return modelRoute{}, false
}

// allModelRoutes returns the routing entries for one account.
func (m clientMenu) allModelRoutes() []modelRoute {
	return append([]modelRoute(nil), m.routes...)
}

// routeFor resolves the model a request asked for against the account's menu.
// The account is the right scope: two accounts can see different menus, and the
// request is about to go out on this one.  When the client has read no menu yet
// -- or the id is not one it advertises -- the module's verified default
// (mode 3 / model item "0") is used, which is the pair the client sends for a
// plain "default" request.
func (c *Client) routeFor(acc *account, model string) modelRoute {
	want := strings.TrimSpace(model)
	if acc != nil && want != "" && want != "default" {
		if menu, ok := c.accountMenu(acc.ID); ok {
			if r, ok := menu.resolveRoute(want); ok {
				return r
			}
		}
	}
	return defaultRoute(c.cfg)
}

// catalogueFromMenu renders the menu as core.Models.  Display and routing
// metadata ride along in Extra so the panel and chat path can use them without
// a second lookup.
func catalogueFromMenu(menu clientMenu, ownedBy string) []core.Model {
	routes := menu.allModelRoutes()
	sort.SliceStable(routes, func(i, j int) bool { return routeID(routes[i]) < routeID(routes[j]) })
	out := make([]core.Model, 0, len(routes))
	for _, r := range routes {
		extra := map[string]any{
			"mode_id":        r.ModeID,
			"model_item_key": r.ModelItemKey,
			"reply_mode":     r.ReplyMode,
		}
		if r.DisplayName != "" {
			extra["name"] = r.DisplayName
		}
		if r.ReasoningEffort != "" {
			extra["reasoning_effort"] = r.ReasoningEffort
		}
		if r.ContextWindow > 0 {
			extra["context_window"] = r.ContextWindow
			extra["context_length"] = r.ContextWindow
		}
		if r.NeedUpgrade {
			extra["need_upgrade"] = true
		}
		out = append(out, core.Model{ID: routeID(r), OwnedBy: ownedBy, Extra: extra})
	}
	return out
}

// contextWindow reads total_window_size from model_extra_params when present.
func contextWindow(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil && strings.HasPrefix(strings.TrimSpace(s), "{") {
			_ = json.Unmarshal([]byte(s), &obj)
		}
	}
	for _, k := range []string{"total_window_size", "context_window", "context_length"} {
		if v, ok := obj[k]; ok {
			return int(parseDecimal(v))
		}
	}
	return 0
}

// parseDecimal reads a JSON number or a numeric string as an int.
func parseDecimal(raw json.RawMessage) int64 {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		if v, err := n.Int64(); err == nil {
			return v
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		var v int64
		if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &v); err == nil {
			return v
		}
	}
	return 0
}

// rawIDString renders a JSON scalar (number or string) as a trimmed string.
func rawIDString(raw json.RawMessage) string {
	return rawScalar(raw)
}

// rawScalar renders a JSON scalar as a string without surrounding quotes.
func rawScalar(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return strings.Trim(strings.TrimSpace(trimmed), `"`)
}
