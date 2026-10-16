package core

import "testing"

// freemodel_test.go pins the rule the router applies when it decides whether a
// model spends credit: an explicit module flag, or a vendor label that names
// the free tier.

func TestModelIsFreeReadsTheExplicitFlag(t *testing.T) {
	m := Model{ID: "glm-5", Extra: map[string]any{"free": true}}
	if !ModelIsFree(m) {
		t.Fatal("an explicit free flag was ignored")
	}
	paid := Model{ID: "glm-5", Extra: map[string]any{"free": false}}
	if ModelIsFree(paid) {
		t.Fatal("an explicit false flag read as free")
	}
}

func TestModelIsFreeReadsVendorLabels(t *testing.T) {
	cases := []struct {
		name  string
		model Model
		want  bool
	}{
		{"display name", Model{ID: "glm-4.6", Extra: map[string]any{"display_name": "GLM-4.6 Free"}}, true},
		{"group name", Model{ID: "glm-4.6", OwnedBy: "Free"}, true},
		{"id group prefix", Model{ID: "cline-free/deepseek-v4.1-flash"}, true},
		{"id free suffix", Model{ID: "vendor/some-model:free"}, true},
		{"chinese label", Model{ID: "glm-4.6", Extra: map[string]any{"display_name": "GLM-4.6 免费"}}, true},
		{"group extra", Model{ID: "glm-4.6", Extra: map[string]any{"name": "Free Tier"}}, true},
		{"paid model", Model{ID: "glm-5.3", OwnedBy: "Zhipu"}, false},
		{"letters alone are not the tier", Model{ID: "freeform-8b", OwnedBy: "Acme"}, false},
	}
	for _, tc := range cases {
		if got := ModelIsFree(tc.model); got != tc.want {
			t.Errorf("%s: ModelIsFree = %v, want %v", tc.name, got, tc.want)
		}
	}
}
