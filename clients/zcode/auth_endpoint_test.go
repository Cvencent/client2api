package zcode

import (
	"os"
	"path/filepath"
	"testing"
)

// A provider entry in the desktop client's config.json is not automatically a
// ZCode account: provider.* is the client's list of places it can send a
// request, and anything Anthropic-shaped can register there.  A local gateway
// does exactly that -- magpie writes provider.magpie with baseURL
// http://127.0.0.1:3425 -- and before this check that entry became a ZCode
// account whose catalogue then polluted this module's model list.  Observed on
// the maintainer's machine: the panel showed `zcode-config:magpie` beside the
// two real plan accounts, and the model list grew 43 entries named
// "My client/...".

func TestIsVendorEndpoint(t *testing.T) {
	cases := []struct {
		base string
		want bool
		why  string
	}{
		// The vendor's own origins, in every shape the client writes them.
		{"https://open.bigmodel.cn/api/anthropic", true, "the mainland API"},
		{"https://api.z.ai/api/anthropic", true, "the international API"},
		{"https://zcode.z.ai/api/v1/zcode-plan/anthropic", true, "the plan channel"},
		{"https://chatglm.cn/api/anthropic", true, "the other mainland host"},
		{"https://BIGMODEL.CN/api/anthropic", true, "host matching is case-insensitive"},
		{"https://open.bigmodel.cn./api/anthropic", true, "a trailing root dot is the same host"},
		// Empty means "the client's default", which is always a vendor origin.
		{"", true, "no baseURL means the default endpoint"},
		{"   ", true, "whitespace is not a URL"},
		// Everything else is somebody else's endpoint.
		{"http://127.0.0.1:3425", false, "a local gateway is not the vendor"},
		{"http://localhost:3425", false, "a local gateway is not the vendor"},
		{"https://relay.example.com/api/anthropic", false, "a third-party relay is not the vendor"},
		{"https://notbigmodel.cn/api/anthropic", false, "a lookalike suffix must not match"},
		{"https://bigmodel.cn.evil.test/api", false, "the vendor host must be the actual host"},
		{"://bad url", false, "an unparseable URL is not accepted"},
	}
	for _, tc := range cases {
		t.Run(tc.base, func(t *testing.T) {
			if got := isVendorEndpoint(tc.base); got != tc.want {
				t.Errorf("isVendorEndpoint(%q) = %v, want %v (%s)", tc.base, got, tc.want, tc.why)
			}
		})
	}
}

// TestProviderDiscoverySkipsALocalGateway is the end-to-end half: a config.json
// holding both the vendor's entry and a local gateway's must yield exactly one
// account.
func TestProviderDiscoverySkipsALocalGateway(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":true}`)
	cfg := `{
	  "provider": {
	    "magpie": {
	      "enabled": true,
	      "kind": "anthropic",
	      "options": {"apiKey": "01ad22d774ec4603a379406ecbd197bb", "baseURL": "http://127.0.0.1:3425"}
	    },
	    "builtin:bigmodel": {
	      "enabled": true,
	      "kind": "anthropic",
	      "options": {"apiKey": "01ad22d774ec4603a379406ecbd197ba", "baseURL": "https://open.bigmodel.cn/api/anthropic"}
	    }
	  }
	}`
	if err := os.MkdirAll(filepath.Dir(env.v2Path("config.json")), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(env.v2Path("config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	var seen []string
	for _, src := range discover(nil) {
		seen = append(seen, src.Label)
	}
	for _, label := range seen {
		if label == "magpie" {
			t.Errorf("a local gateway entry became a ZCode account; got %v", seen)
		}
	}
	found := false
	for _, label := range seen {
		if label == "builtin:bigmodel" {
			found = true
		}
	}
	if !found {
		t.Errorf("the vendor's own entry was dropped; got %v", seen)
	}
}
