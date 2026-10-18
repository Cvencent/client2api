package panel

import (
	"strings"
	"testing"
)

func TestModelsViewHasExposedModelsSubtab(t *testing.T) {
	src := poolStatsUISource(t)
	nav := subtabNavHTML(t, src, "models")
	for _, want := range []string{
		`data-subtab="capability"`, ">模型能力<",
		`data-subtab="exposed"`, ">对外模型<",
		`role="tab"`, `aria-selected="true"`, `class="chip subtab on"`,
	} {
		if !strings.Contains(nav, want) {
			t.Errorf("模型与档位二级 Tab 缺少 %q", want)
		}
	}
	for _, tc := range []struct {
		panel string
		ids   []string
	}{
		{"models-capability", []string{`id="mdBody"`, `id="btnMdSave"`}},
		{"models-exposed", []string{`id="exBody"`, `id="exSearch"`, `id="exModeAll"`, `id="exModeSelected"`, `id="btnExSave"`}},
	} {
		panel := subtabPanelHTML(t, src, tc.panel)
		for _, id := range tc.ids {
			if !strings.Contains(panel, id) {
				t.Errorf("模型面板 %s 不包含 %s", tc.panel, id)
			}
		}
	}
	if strings.Contains(subtabOpenTag(t, src, "models-capability"), " hidden") {
		t.Error("模型能力应该是默认可见面板")
	}
	if !strings.Contains(subtabOpenTag(t, src, "models-exposed"), " hidden") {
		t.Error("对外模型面板默认应隐藏")
	}
}

func TestExposedModelsUIWiresFetchAndHotSave(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		`setupSubtabs("models"`,
		`api("/panel/api/exposed_models")`,
		`exposed_models: EX_MODE === "all" ? null : selected`,
		`api("/panel/api/config", { method: "PATCH", body: body })`,
		`api("/panel/api/reload", { method: "POST" })`,
		`function exToggleVisible`,
		`function exSave`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("对外模型 UI 接线缺少 %q", want)
		}
	}
}

// TestChatTesterSendsTheInternalMarker pins the UI half of the fix: the panel's
// chat tester must mark its own /v1/models and /v1/chat/completions calls, or
// the gateway would keep restricting the model picker to the exposed_models
// whitelist -- the very list the tester is supposed to look past.
func TestChatTesterSendsTheInternalMarker(t *testing.T) {
	src := poolStatsUISource(t)

	models := poolStatsFuncBody(t, src, "chatModels")
	if !strings.Contains(models, `"/v1/models"`) {
		t.Fatalf("chatModels no longer calls /v1/models:\n%s", models)
	}
	if !strings.Contains(models, "X-Client2API-Internal-Test") {
		t.Error("chatModels must send the internal-test marker so the picker is not whitelisted")
	}

	send := poolStatsFuncBody(t, src, "chatSend")
	if !strings.Contains(send, "/v1/chat/completions") {
		t.Fatalf("chatSend no longer posts a completion:\n%s", send)
	}
	if !strings.Contains(send, "X-Client2API-Internal-Test") {
		t.Error("chatSend must send the internal-test marker so an unexposed model can still be tested")
	}

	finish := poolStatsFuncBody(t, src, "chatTranscribe")
	if !strings.Contains(finish, "/v1/audio/transcriptions") {
		t.Fatalf("chatTranscribe no longer posts a transcription:\n%s", finish)
	}
	if !strings.Contains(finish, "X-Client2API-Internal-Test") {
		t.Error("the transcription test must send the internal-test marker too")
	}
}
