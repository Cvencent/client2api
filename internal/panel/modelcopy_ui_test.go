package panel

import (
	"strings"
	"testing"
)

// TestModelsViewCopiesFullClientModelName pins the model table's copy
// affordance: operators need the exact routed id (client/model), not the
// friendly display name shown in the cell.
func TestModelsViewCopiesFullClientModelName(t *testing.T) {
	src := poolStatsUISource(t)

	render := poolStatsFuncBody(t, src, "renderModels")
	if !strings.Contains(render, "mdCopyButton(c.name, m.id)") {
		t.Fatal("renderModels does not render a copy button for each model")
	}

	button := poolStatsFuncBody(t, src, "mdCopyButton")
	for _, want := range []string{
		`client + "/" + model`,
		`data-copy-model="' + esc(full)`,
		`title="复制完整模型名"`,
		`>复制</button>`,
	} {
		if !strings.Contains(button, want) {
			t.Errorf("mdCopyButton is missing %q", want)
		}
	}

	handlerStart := strings.Index(src, `$("#mdBody").addEventListener("click"`)
	if handlerStart < 0 {
		t.Fatal("mdBody has no click handler")
	}
	handlerTail := src[handlerStart:]
	handlerEnd := strings.Index(handlerTail, "\n});")
	if handlerEnd < 0 {
		t.Fatal("mdBody click handler is not terminated")
	}
	handler := handlerTail[:handlerEnd]
	for _, want := range []string{
		`.closest(".mdCopy")`,
		`dataset.copyModel`,
		`copyText(full)`,
		`toast("已复制客户端/模型名"`,
	} {
		if !strings.Contains(handler, want) {
			t.Errorf("mdBody copy handler is missing %q", want)
		}
	}
	if copyAt, restoreAt := strings.Index(handler, `.closest(".mdCopy")`), strings.Index(handler, `.closest(".mdRestore")`); copyAt < 0 || restoreAt < 0 || copyAt > restoreAt {
		t.Error("copy handling must run before the restore-default branch")
	}
}
