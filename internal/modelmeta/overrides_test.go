package modelmeta

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOverrideStorePersistsAndDeletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_context.json")
	s, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("zcode", "glm-5.3-flash", 1048576, 131072); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("zcode", "glm-5.3-flash")
	if !ok || got.ContextLength != 1048576 || got.MaxOutputTokens != 131072 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}

	reopened, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Get("zcode", "glm-5.3-flash"); !ok || got.ContextLength != 1048576 || got.MaxOutputTokens != 131072 {
		t.Fatalf("reopened %+v ok=%v", got, ok)
	}
	if err := reopened.Set("zcode", "glm-5.3-flash", 0, 131072); err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Get("zcode", "glm-5.3-flash"); !ok || got.ContextLength != 0 || got.MaxOutputTokens != 131072 {
		t.Fatalf("partial update %+v ok=%v", got, ok)
	}
	if err := reopened.Delete("zcode", "glm-5.3-flash"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get("zcode", "glm-5.3-flash"); ok {
		t.Fatal("delete did not remove entry")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" {
		t.Fatal("store wrote an empty file")
	}
}

// TestOverrideStoreReplaceRestoresAnExportedSnapshot pins the bundle path:
// restoring a snapshot must replace every override (including entries the
// target had that the bundle does not) and persist it for the next start.
func TestOverrideStoreReplaceRestoresAnExportedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_context.json")
	s, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("zcode", "stale", 1, 2); err != nil {
		t.Fatal(err)
	}
	snapshot := map[string]map[string]Override{
		"zcode": {"GLM-5.3": {ContextLength: 200000, MaxOutputTokens: 8192}},
	}
	if err := s.Replace(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("zcode", "stale"); ok {
		t.Fatal("Replace kept an override the snapshot did not carry")
	}
	got, ok := s.Get("zcode", "GLM-5.3")
	if !ok || got.ContextLength != 200000 || got.MaxOutputTokens != 8192 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	reopened, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Get("zcode", "GLM-5.3"); !ok || got.ContextLength != 200000 {
		t.Fatalf("reopened %+v ok=%v", got, ok)
	}
}

func TestOverrideStoreCorruptFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_context.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warned bool
	s, err := OpenOverrideStore(path, func(string, ...any) { warned = true })
	if err != nil {
		t.Fatal(err)
	}
	if !warned {
		t.Fatal("corrupt file did not warn")
	}
	if _, ok := s.Get("zcode", "glm-5.3-flash"); ok {
		t.Fatal("corrupt file produced an entry")
	}
}

func TestOverrideStorePersistsEffortPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_context.json")
	s, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEfforts("workbuddy", "cn:glm-5.3", []string{"low", "High", "max", "max"}, " MAX "); err != nil {
		t.Fatal(err)
	}
	if err := s.SetElevateTop("workbuddy", "cn:glm-5.3", true); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get("workbuddy", "cn:glm-5.3")
	if !ok {
		t.Fatal("effort policy did not persist")
	}
	if !equalStrings(got.SupportedEfforts, []string{"low", "high", "max"}) {
		t.Fatalf("supported efforts = %v, want [low high max]", got.SupportedEfforts)
	}
	if got.DefaultEffort != "max" {
		t.Fatalf("default effort = %q, want max", got.DefaultEffort)
	}
	if !got.ElevateTop {
		t.Fatal("elevate_top = false, want true")
	}
}

// TestOverrideStoreLegacyElevateTopCountsAsExplicit pins the upgrade path: the
// first release of cap alignment wrote only elevate_top:true. That entry was an
// operator decision, so it must keep overriding a model whose embedded default
// later changes instead of being read as "never configured".
func TestOverrideStoreLegacyElevateTopCountsAsExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_context.json")
	legacy := `{"clients":{"loomy":{"DeepSeek-V4.1-Flash":{"elevate_top":true}}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("loomy", "DeepSeek-V4.1-Flash")
	if !ok {
		t.Fatal("legacy entry was dropped")
	}
	on, explicit := got.ExplicitElevateTop()
	if !on || !explicit {
		t.Fatalf("legacy ExplicitElevateTop = (%v, %v), want (true, true)", on, explicit)
	}
	if !ElevateTop("loomy", "DeepSeek-V4.1-Flash", s) {
		t.Fatal("legacy elevate_top:true resolved to off")
	}
}

// TestOverrideStoreExplicitElevateTopOffBeatsEmbeddedDefault pins the other
// half of the precedence chain: a deliberate "off" survives a model whose
// embedded table now defaults the policy on, and "restore default" brings the
// embedded default back instead of pinning a permanent off.
func TestOverrideStoreExplicitElevateTopOffBeatsEmbeddedDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_context.json")
	s, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !StaticElevateTop("loomy", "DeepSeek-V4.1-Flash") {
		t.Fatal("the fixture model must default the policy on")
	}
	if err := s.SetElevateTop("loomy", "DeepSeek-V4.1-Flash", false); err != nil {
		t.Fatal(err)
	}
	if ElevateTop("loomy", "DeepSeek-V4.1-Flash", s) {
		t.Fatal("an explicit off did not beat the embedded default")
	}
	reopened, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get("loomy", "DeepSeek-V4.1-Flash")
	if !ok {
		t.Fatal("the explicit off was not persisted")
	}
	if on, explicit := got.ExplicitElevateTop(); on || !explicit {
		t.Fatalf("persisted ExplicitElevateTop = (%v, %v), want (false, true)", on, explicit)
	}
	if ElevateTop("loomy", "DeepSeek-V4.1-Flash", reopened) {
		t.Fatal("the persisted explicit off did not beat the embedded default")
	}
	if err := reopened.ClearElevateTop("loomy", "DeepSeek-V4.1-Flash"); err != nil {
		t.Fatal(err)
	}
	if !ElevateTop("loomy", "DeepSeek-V4.1-Flash", reopened) {
		t.Fatal("restore default did not fall back to the embedded default")
	}
}

func TestOverrideStorePersistsMultipleEffortMappings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_context.json")
	s, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEffortMappings("workbuddy", "cn:glm-5.3-flash", []EffortMapping{
		{From: " HIGH ", To: " MAX "},
		{From: "xhigh", To: "top"},
	}); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenOverrideStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get("workbuddy", "cn:glm-5.3-flash")
	if !ok {
		t.Fatal("effort mappings did not persist")
	}
	if len(got.EffortMappings) != 2 {
		t.Fatalf("effort mappings = %+v, want two rules", got.EffortMappings)
	}
	if got.EffortMappings[0] != (EffortMapping{From: "high", To: "max"}) {
		t.Fatalf("first mapping = %+v, want high -> max", got.EffortMappings[0])
	}
	if got.EffortMappings[1] != (EffortMapping{From: "xhigh", To: "top"}) {
		t.Fatalf("second mapping = %+v, want xhigh -> top", got.EffortMappings[1])
	}
}

func TestOverrideStoreRejectsDuplicateEffortMappingSources(t *testing.T) {
	s, err := OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	err = s.SetEffortMappings("workbuddy", "cn:glm-5.3-flash", []EffortMapping{
		{From: "high", To: "max"},
		{From: "HIGH", To: "xhigh"},
	})
	if err == nil {
		t.Fatal("duplicate source was accepted")
	}
}

func TestEffortMappingHighestAliasNormalizesToTop(t *testing.T) {
	s, err := OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEffortMappings("workbuddy", "cn:glm-5.3-flash", []EffortMapping{{From: "high", To: " HIGHEST "}}); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("workbuddy", "cn:glm-5.3-flash")
	if !ok || len(got.EffortMappings) != 1 {
		t.Fatalf("mapping = %+v ok=%v", got.EffortMappings, ok)
	}
	if got.EffortMappings[0] != (EffortMapping{From: "high", To: EffortMappingTop}) {
		t.Fatalf("mapping = %+v, want high -> top", got.EffortMappings[0])
	}
}
