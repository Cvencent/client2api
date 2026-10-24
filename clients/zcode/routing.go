package zcode

// The vendor retired the direct chat endpoints.  /api/v1/agent/configs now
// publishes a rewrite table (proxyEndpoint.mapping) that the official client
// applies before sending: the api-key and plan hosts are rewritten onto
// /api/v1/ultra* and /api/v1/ultra-zai*, and those requests carry
// X-Zcode-Authorization.
//
// The retired URLs are not merely 404: they answer HTTP 405 with envelope code
// 3012 ("unusual activity") -- the same shape as a risk-control block.  Posting
// to them therefore looked like risk control and fed the shared IP-level gate,
// which is how one dead route parked every other client on the machine.
//
// The table below is transcribed from the vendor document (2026-10-24) and
// kept as a compiled-in default, the same way the base URLs in config.go are:
// it is a protocol fact, and consulting it must never add a request to the chat
// path.  parseAgentConfigs exists so the vendor document can be re-read and
// diffed against this table when the route changes again -- see routing_test.go.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const (
	// ultraBigmodel is the current chat route for a BigModel credential.
	ultraBigmodel = "https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages"
	// ultraZai is the current chat route for a Z.AI credential.
	ultraZai = "https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages"

	// headerZCodeAuth is the header the vendor mapping names for a routed
	// request.  The official client sends the plan credential here instead of
	// Authorization.
	headerZCodeAuth = "X-Zcode-Authorization"
)

// routingEntry is one proxyEndpoint.mapping row.
type routingEntry struct {
	URL              string
	IncludeZCodeAuth bool
}

// vendorRoutedMessages maps each retired chat URL onto the route the vendor
// serves now.  Keys are normalised via normalizeRouteURL.
var vendorRoutedMessages = map[string]routingEntry{
	normalizeRouteURL("https://open.bigmodel.cn/api/anthropic/v1/messages"): {
		URL:              ultraBigmodel,
		IncludeZCodeAuth: true,
	},
	normalizeRouteURL("https://api.z.ai/api/anthropic/v1/messages"): {
		URL:              ultraZai,
		IncludeZCodeAuth: true,
	},
}

// agentConfigs is the wire shape of the vendor routing document.  Only the
// fields this module acts on are declared; the rest is ignored on purpose so a
// vendor addition cannot break the decode.
type agentConfigs struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		ProxyEndpoint struct {
			Mapping []struct {
				From                     string `json:"from"`
				To                       string `json:"to"`
				ZcodeAuthorizationHeader string `json:"zcodeAuthorizationHeader"`
			} `json:"mapping"`
		} `json:"proxyEndpoint"`
	} `json:"data"`
}

// parseAgentConfigs turns the vendor document into a from->target table.  A
// non-zero envelope code is an error rather than an empty table: treating a
// refusal as "no routes" would silently fall back to the retired URLs.
func parseAgentConfigs(raw []byte) (map[string]routingEntry, error) {
	var doc agentConfigs
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode the routing document: %w", err)
	}
	if doc.Code != 0 {
		return nil, fmt.Errorf("the routing document reported code %d: %s", doc.Code, strings.TrimSpace(doc.Msg))
	}
	out := make(map[string]routingEntry, len(doc.Data.ProxyEndpoint.Mapping))
	for _, row := range doc.Data.ProxyEndpoint.Mapping {
		from := normalizeRouteURL(row.From)
		to := normalizeRouteURL(row.To)
		if from == "" || to == "" {
			continue
		}
		out[from] = routingEntry{
			URL:              to,
			IncludeZCodeAuth: strings.TrimSpace(row.ZcodeAuthorizationHeader) != "",
		}
	}
	return out, nil
}

// routeFor rewrites one chat URL through the vendor table.  An unmapped URL is
// returned unchanged with include=false, which is what keeps an operator
// base_url override (a local gateway, a test server) untouched.
func routeFor(rawURL string) (string, bool) {
	entry, ok := vendorRoutedMessages[normalizeRouteURL(rawURL)]
	if !ok || entry.URL == "" {
		return rawURL, false
	}
	return entry.URL, entry.IncludeZCodeAuth
}

// normalizeRouteURL canonicalises a URL for table matching: lowercase scheme
// and host, no trailing slash.
func normalizeRouteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.TrimRight(raw, "/")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if u.Path != "/" {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	return u.String()
}
