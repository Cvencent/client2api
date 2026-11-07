package openaicompat

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// #238：测试探针必须尊重操作员的模型策略——导入时的 probe_models 快照看不到
// 后来的勾选，禁用的模型不进探测候选；全部候选被禁时回退原清单（结果可读，
// 空跑不可读），并带 Note 注明收窄。
// ---------------------------------------------------------------------------

func TestWithProbeFilterRemovesDisabledAndKeepsTheRest(t *testing.T) {
	p := ProviderConfig{ProbeModels: []string{"gemini-3.8-flash", "gemini-3.8-flash-1017", "glm-5"}}
	got := p.withProbeFilter(p.ProbeModels, []string{"GEMINI-3.8-FLASH"})
	if len(p.ProbeModels) != 3 {
		t.Fatalf("the original slice was mutated: %v", p.ProbeModels)
	}
	kept := got.ProbeModels
	if len(kept) != 2 || kept[0] != "gemini-3.8-flash-1017" || kept[1] != "glm-5" {
		t.Fatalf("kept = %v, want the disabled one removed case-insensitively", kept)
	}
	if got.probeFiltered == "" {
		t.Error("narrowed probe did not record the annotation")
	}
}

func TestWithProbeFilterFallsBackWhenEverythingIsDisabled(t *testing.T) {
	p := ProviderConfig{ProbeModels: []string{"m1", "m2"}}
	got := p.withProbeFilter(p.ProbeModels, []string{"m1", "M2"})
	if got.probeFiltered != "" {
		t.Errorf("fallback recorded an annotation: %q", got.probeFiltered)
	}
	if len(got.ProbeModels) != 2 {
		t.Fatalf("fallback list = %v, want the unfiltered original", got.ProbeModels)
	}
}

func TestWithProbeFilterNoPolicyIsIdentity(t *testing.T) {
	p := ProviderConfig{ProbeModels: []string{"m1", "m2"}}
	got := p.withProbeFilter(p.ProbeModels, nil)
	if got.probeFiltered != "" || len(got.ProbeModels) != 2 {
		t.Fatalf("no-policy path changed the probe list: %+v note=%q", got.ProbeModels, got.probeFiltered)
	}
	// 候选未命中任何禁用项时也必须是恒等（不改 ProbeModels）。
	got2 := p.withProbeFilter(p.ProbeModels, []string{"other-model"})
	if got2.probeFiltered != "" || len(got2.ProbeModels) != 2 {
		t.Fatalf("unmatched policy changed the probe list: %+v note=%q", got2.ProbeModels, got2.probeFiltered)
	}
}

// TestProbeProviderAnnotatesNarrowedRuns 钉住 probeProvider 的标注出口：
// 带收窄标记的 provider 跑出的结果必须带 Note。
func TestProbeProviderAnnotatesNarrowedRuns(t *testing.T) {
	c := newTestClient(t, t.TempDir())
	prov := ProviderConfig{probeFiltered: "probe list narrowed by model policy"}
	rec := providerRecord{ID: "acct-1"}
	// 无 API key 的提前返回也必须先带上 Note：这是结果形状的一部分。
	res := c.probeProvider(context.Background(), prov, rec)
	if res.Note != "探测清单已按模型策略收窄" {
		t.Fatalf("Note = %q, want the narrowed annotation (issue #238)", res.Note)
	}
}
