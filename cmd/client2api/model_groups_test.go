package main

import (
	"encoding/json"
	"testing"
)

func TestModelGroupsProjection(t *testing.T) {
	cfg := &fileConfig{ModelGroups: map[string]modelGroupConfig{
		"DeepSeek-V4.1-Flash": {
			Members: []string{
				"opencode/deepseek-v4.1-flash",
				"cline/cline-free/deepseek-v4.1-flash",
				"",
				"missing-slash",
				"  workbuddy/cn:deepseek-v4.1-flash  ",
			},
			PlatformPriorities: map[string]int{
				"opencode": 10,
				"cline":    20,
				"  ":       99,
			},
		},
	}}

	got := cfg.modelGroups()
	group, ok := got["deepseek-v4.1-flash"]
	if !ok {
		t.Fatalf("modelGroups() keys = %v, want the normalized group name", got)
	}
	if len(group.Members) != 3 {
		t.Fatalf("members = %+v, want three valid client/model rows", group.Members)
	}
	if group.Members[0].Client != "opencode" || group.Members[0].Model != "deepseek-v4.1-flash" {
		t.Fatalf("first member = %+v, want opencode/deepseek-v4.1-flash", group.Members[0])
	}
	if group.Members[1].Client != "cline" || group.Members[1].Model != "cline-free/deepseek-v4.1-flash" {
		t.Fatalf("second member = %+v, want cline/cline-free/deepseek-v4.1-flash", group.Members[1])
	}
	if group.Members[2].Client != "workbuddy" || group.Members[2].Model != "cn:deepseek-v4.1-flash" {
		t.Fatalf("third member = %+v, want trimmed workbuddy/cn:deepseek-v4.1-flash", group.Members[2])
	}
	if group.PlatformPriorities["opencode"] != 10 || group.PlatformPriorities["cline"] != 20 {
		t.Fatalf("priorities = %v, want the two non-blank keys", group.PlatformPriorities)
	}
	if _, exists := group.PlatformPriorities[""]; exists {
		t.Fatalf("blank priority key survived: %v", group.PlatformPriorities)
	}
}

func TestModelGroupsJSONRoundTrip(t *testing.T) {
	raw := []byte(`{
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

	var parsed fileConfig
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	group := parsed.ModelGroups["deepseek-v4.1-flash"]
	if len(group.Members) != 2 || group.PlatformPriorities["opencode"] != 10 {
		t.Fatalf("model_groups did not round-trip: %+v", group)
	}
}
