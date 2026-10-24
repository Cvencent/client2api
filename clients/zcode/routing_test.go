package zcode

import "testing"

// The vendor retired the direct chat endpoints: /api/v1/agent/configs now
// publishes a rewrite table (proxyEndpoint.mapping) that the official client
// applies before sending, and the retired URLs answer 405 code 3012.

const agentConfigsFixture = `{"code":0,"data":{"codingPlanSignature":{"enable":true},` +
	`"proxyEndpoint":{"mapping":[` +
	`{"from":"https://open.bigmodel.cn/api/anthropic/v1/messages",` +
	`"to":"https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages",` +
	`"zcodeAuthorizationHeader":"X-Zcode-Authorization"},` +
	`{"from":"https://api.z.ai/api/anthropic/v1/messages",` +
	`"to":"https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages",` +
	`"zcodeAuthorizationHeader":"X-Zcode-Authorization"}]}}}`

func TestParseAgentConfigs(t *testing.T) {
	table, err := parseAgentConfigs([]byte(agentConfigsFixture))
	if err != nil {
		t.Fatalf("parseAgentConfigs: %v", err)
	}
	if len(table) != 2 {
		t.Fatalf("mapping entries = %d, want 2", len(table))
	}
	bm, ok := table["https://open.bigmodel.cn/api/anthropic/v1/messages"]
	if !ok {
		t.Fatalf("bigmodel route missing; have %v", table)
	}
	if bm.URL != "https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages" {
		t.Errorf("bigmodel target = %q", bm.URL)
	}
	if !bm.IncludeZCodeAuth {
		t.Error("bigmodel route must carry X-Zcode-Authorization")
	}
	zai, ok := table["https://api.z.ai/api/anthropic/v1/messages"]
	if !ok || zai.URL != "https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages" {
		t.Errorf("zai route = %+v ok=%v", zai, ok)
	}
	if !zai.IncludeZCodeAuth {
		t.Error("zai route must carry X-Zcode-Authorization")
	}
}

// A refusal must be an error, never an empty table: an empty table would look
// exactly like "the vendor publishes no routes" and keep the retired URLs.
func TestParseAgentConfigsRejectsRefusal(t *testing.T) {
	if _, err := parseAgentConfigs([]byte(`{"code":500,"msg":"nope"}`)); err == nil {
		t.Fatal("a non-zero envelope code must be an error")
	}
	if _, err := parseAgentConfigs([]byte(`not json`)); err == nil {
		t.Fatal("invalid JSON must be an error")
	}
}

func TestRouteForRewritesChatURL(t *testing.T) {
	got, include := routeFor("https://open.bigmodel.cn/api/anthropic/v1/messages")
	if got != "https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages" {
		t.Errorf("routed url = %q", got)
	}
	if !include {
		t.Error("a routed request must include X-Zcode-Authorization")
	}
}

// Fail-open: an unmapped URL -- an operator base_url override, a test server,
// anything not in the vendor table -- is used verbatim and gains no auth header.
func TestRouteForIsFailOpen(t *testing.T) {
	if got, include := routeFor("http://127.0.0.1:3000/v1/messages"); got != "http://127.0.0.1:3000/v1/messages" || include {
		t.Errorf("base_url override = %q include=%v", got, include)
	}
	if got, include := routeFor("https://example.com/v1/messages"); got != "https://example.com/v1/messages" || include {
		t.Errorf("unmapped = %q include=%v", got, include)
	}
	if got, include := routeFor(""); got != "" || include {
		t.Errorf("empty = %q include=%v", got, include)
	}
}

// Trailing slashes and host casing must not defeat a lookup.
func TestNormalizeRouteURL(t *testing.T) {
	cases := map[string]string{
		"https://Open.BigModel.cn/api/anthropic/v1/messages/": "https://open.bigmodel.cn/api/anthropic/v1/messages",
		"  https://api.z.ai/api/anthropic/v1/messages":        "https://api.z.ai/api/anthropic/v1/messages",
		"": "",
	}
	for in, want := range cases {
		if got := normalizeRouteURL(in); got != want {
			t.Errorf("normalizeRouteURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The compiled-in table must agree with the vendor document shape it was
// transcribed from.  This is the guard that turns a future route change into a
// failing test rather than a silent 3012 in production.
func TestVendoredTableMatchesParsedDocument(t *testing.T) {
	parsed, err := parseAgentConfigs([]byte(agentConfigsFixture))
	if err != nil {
		t.Fatalf("parseAgentConfigs: %v", err)
	}
	for from, want := range parsed {
		got, ok := vendorRoutedMessages[from]
		if !ok {
			t.Errorf("vendor publishes route %q -> %q but vendorRoutedMessages has no entry", from, want.URL)
			continue
		}
		if got.URL != want.URL {
			t.Errorf("route %q target = %q, vendor document says %q", from, got.URL, want.URL)
		}
		if got.IncludeZCodeAuth != want.IncludeZCodeAuth {
			t.Errorf("route %q IncludeZCodeAuth = %v, vendor says %v", from, got.IncludeZCodeAuth, want.IncludeZCodeAuth)
		}
	}
	if len(parsed) != len(vendorRoutedMessages) {
		t.Errorf("vendor publishes %d routes, vendored table has %d", len(parsed), len(vendorRoutedMessages))
	}
}
