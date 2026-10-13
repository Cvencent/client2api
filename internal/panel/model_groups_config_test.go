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

// Model_group names are case-insensitive (the registry lowercases them and the
// validator compares them case-insensitively), but the on-disk key keeps the
// operator's original casing. A save from the panel echoes the normalized
// lowercase name, so the merge must fold it onto the existing key instead of
// adding a second, duplicate-looking entry.
func TestConfigPatchFoldsMixedCaseModelGroupOntoExistingKey(t *testing.T) {
	path := configFile(t, `{
		"listen": "127.0.0.1:8788",
		"model_groups": {"DeepSeek-Flash": {"members": ["Auto/deepseek-v4.1-flash"]}},
		"clients": {}
	}`)
	mustSave(t, configPanel(path), `{"model_groups":{"deepseek-flash":{"members":[
		"Auto/deepseek-v4.1-flash",
		"lobsterai/deepseek-v4-flash",
		"tokenrhythm/deepseek-flash",
		"raccoon/sn-deepseek-v4-1-flash",
		"workbuddy/global:deepseek-v4.1-flash"
	]}}}`)

	groups := onDisk(t, path)["model_groups"].(map[string]any)
	if len(groups) != 1 {
		t.Fatalf("model_groups = %v, want the mixed-case key folded onto one entry", groups)
	}
	group, ok := groups["DeepSeek-Flash"].(map[string]any)
	if !ok {
		t.Fatalf("the on-disk key casing was not preserved: %v", groups)
	}
	members, _ := group["members"].([]any)
	want := []string{
		"Auto/deepseek-v4.1-flash",
		"lobsterai/deepseek-v4-flash",
		"tokenrhythm/deepseek-flash",
		"raccoon/sn-deepseek-v4-1-flash",
		"workbuddy/global:deepseek-v4.1-flash",
	}
	if len(members) != len(want) {
		t.Fatalf("members = %v, want %v", members, want)
	}
	for i, m := range members {
		if m != want[i] {
			t.Fatalf("members[%d] = %v, want %v", i, m, want[i])
		}
	}
}

// Deleting a group sends a null for the panel's normalized lowercase name; it
// has to remove the mixed-case key that is actually on disk.
func TestConfigPatchDeletesMixedCaseModelGroupByNormalizedName(t *testing.T) {
	path := configFile(t, `{
		"listen": "127.0.0.1:8788",
		"model_groups": {"DeepSeek-Flash": {"members": ["Auto/deepseek-v4.1-flash"]}},
		"clients": {}
	}`)
	mustSave(t, configPanel(path), `{"model_groups":{"deepseek-flash":null}}`)

	groups, _ := onDisk(t, path)["model_groups"].(map[string]any)
	if _, stale := groups["DeepSeek-Flash"]; stale {
		t.Fatalf("the mixed-case group survived a delete: %v", groups)
	}
	if len(groups) != 0 {
		t.Fatalf("model_groups = %v, want it empty after the delete", groups)
	}
}
