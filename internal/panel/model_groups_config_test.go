package panel

import (
	"net/http"
	"strings"
	"testing"
)

func TestConfigPatchCarriesModelRoutingGroups(t *testing.T) {
	path := configFile(t, baseConfig)
	out := mustSave(t, configPanel(path), `{
		"model_groups": {
			"deepseek-v4.1-flash": {
				"members": [
					"opencode/deepseek-v4.1-flash",
					"cline/cline-free/deepseek-v4.1-flash"
				],
				"platform_priorities": {
					"opencode": 10,
					"cline": 20
				}
			}
		}
	}`)

	changed, _ := out["changed"].([]any)
	if len(changed) != 1 || changed[0] != "model_groups" {
		t.Fatalf("changed = %v, want [model_groups]", out["changed"])
	}

	got := onDisk(t, path)
	groups, ok := got["model_groups"].(map[string]any)
	if !ok {
		t.Fatalf("model_groups was not saved as an object: %v", got["model_groups"])
	}
	group, _ := groups["deepseek-v4.1-flash"].(map[string]any)
	if group == nil {
		t.Fatalf("group disappeared: %v", groups)
	}
	members, _ := group["members"].([]any)
	if len(members) != 2 || members[0] != "opencode/deepseek-v4.1-flash" {
		t.Fatalf("members = %v, want the two configured rows", group["members"])
	}
	priorities, _ := group["platform_priorities"].(map[string]any)
	if priorities["opencode"] != float64(10) || priorities["cline"] != float64(20) {
		t.Fatalf("priorities = %v, want opencode=10 cline=20", priorities)
	}

	text := readFileString(t, path)
	if strings.Index(text, `"model_groups"`) > strings.Index(text, `"clients"`) {
		t.Fatalf("model_groups should be written near the routing keys, before clients:\n%s", text)
	}
}

func TestConfigPatchDeletesOneModelGroupPriorityWithoutStaleMerge(t *testing.T) {
	path := configFile(t, `{
		"model_groups": {
			"shared": {
				"members": ["alpha/a", "beta/b"],
				"platform_priorities": {"alpha": 5, "beta": 7}
			}
		}
	}`)
	mustSave(t, configPanel(path), `{"model_groups":{"shared":{"platform_priorities":{"alpha":null}}}}`)

	groups := onDisk(t, path)["model_groups"].(map[string]any)
	shared := groups["shared"].(map[string]any)
	priorities, _ := shared["platform_priorities"].(map[string]any)
	if _, stale := priorities["alpha"]; stale {
		t.Fatalf("alpha priority survived a null patch: %v", priorities)
	}
	if priorities["beta"] != float64(7) {
		t.Fatalf("beta priority was lost: %v", priorities)
	}
}

func TestConfigPatchRejectsMalformedModelGroups(t *testing.T) {
	cases := []struct {
		name  string
		patch string
		want  string
	}{
		{"not an object", `{"model_groups":[]}`, "model_groups must be an object"},
		{"blank group name", `{"model_groups":{"  ":{"members":["alpha/a"]}}}`, "model group name"},
		{"slash group name", `{"model_groups":{"a/b":{"members":["alpha/a"]}}}`, "must not contain"},
		{"empty member list", `{"model_groups":{"shared":{"members":[]}}}`, "non-empty"},
		{"member not a string", `{"model_groups":{"shared":{"members":[5]}}}`, "members must contain only strings"},
		{"member without slash", `{"model_groups":{"shared":{"members":["alpha"]}}}`, "client/model"},
		{"member with blank client", `{"model_groups":{"shared":{"members":["/a"]}}}`, "client/model"},
		{"duplicate member in one group", `{"model_groups":{"shared":{"members":["Alpha/A","alpha/a"]}}}`, "duplicate member"},
		{"duplicate member across groups", `{"model_groups":{"one":{"members":["alpha/a"]},"two":{"members":["ALPHA/A"]}}}`, "duplicate member"},
		{"case-only duplicate group", `{"model_groups":{"Shared":{"members":["alpha/a"]},"shared":{"members":["beta/b"]}}}`, "duplicate model group"},
		{"alias collision", `{"model_groups":{"GPT-4O":{"members":["alpha/a"]}}}`, "collides with alias"},
		{"priority not an object", `{"model_groups":{"shared":{"members":["alpha/a"],"platform_priorities":[]}}}`, "platform_priorities must be an object"},
		{"priority unknown platform", `{"model_groups":{"shared":{"members":["alpha/a"],"platform_priorities":{"beta":1}}}}`, "not a member"},
		{"priority fractional", `{"model_groups":{"shared":{"members":["alpha/a"],"platform_priorities":{"alpha":1.5}}}}`, "whole number"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := configFile(t, baseConfig)
			w, out := doConfig(t, configPanel(path), http.MethodPatch, tc.patch)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("PATCH = %d, want 400 (%s)", w.Code, w.Body.String())
			}
			msg, _ := out["error"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("error %q does not mention %q", msg, tc.want)
			}
		})
	}
}
