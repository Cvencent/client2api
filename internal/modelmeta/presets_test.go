package modelmeta

import "testing"

// TestStaticTableCoversPlatformModels pins one representative model per client
// whose upstream vendor publishes an official window/output. The values are the
// vendor's own numbers; a model with no public source is deliberately absent
// (see TestStaticUnknownModelsAreNotFound).
func TestStaticTableCoversPlatformModels(t *testing.T) {
	cases := []struct {
		client  string
		model   string
		context int64
		output  int64
	}{
		{"kimi", "k3-agent", 1000000, 0},
		{"lobsterai", "deepseek-flash", 1000000, 0},
		{"minimaxcode", "MiniMax-M3", 512000, 128000},
		{"zcode", "glm-5.3-flash", 1000000, 128000},
		{"autoclaw", "zaicoding_glm-5.3", 1048576, 307200},
	}
	for _, c := range cases {
		m, ok := StaticMeta(c.client, RealmCN, c.model)
		if !ok {
			t.Errorf("%s/%s missing from the static table", c.client, c.model)
			continue
		}
		if m.ContextLength != c.context || m.MaxOutputTokens != c.output {
			t.Errorf("%s/%s = %d/%d, want %d/%d", c.client, c.model, m.ContextLength, m.MaxOutputTokens, c.context, c.output)
		}
		if m.Source != SourceStatic {
			t.Errorf("%s/%s source = %q, want %q", c.client, c.model, m.Source, SourceStatic)
		}
	}
}
