package panel

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestModelGroupPriorityEditsApplyToAllPlatformMembers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	src := poolStatsUISource(t)
	read := poolStatsFuncBody(t, src, "pfReadModelGroups") + "\n}"
	listener := ""
	if start := strings.Index(src, `$("#pfGroups").addEventListener("input"`); start >= 0 {
		rest := src[start:]
		if end := strings.Index(rest, "\n});"); end >= 0 {
			listener = rest[:end+4]
		}
	}
	code, _ := json.Marshal(read + "\n" + listener)
	program := `
const assert = require("assert");
let handler;
const group = {
  querySelector: () => ({value:"group"}),
  querySelectorAll: () => rows
};
function row(member, priority) {
  const m = {value:member}, p = {value:priority};
  const r = {querySelector:s=>s===".pfGMember"?m:p};
  p.closest = s => s === ".pfGPrio" ? p : (s === ".pfGroup" ? group : (s === ".pfGRow" ? r : null));
  m.closest = s => s === ".pfGMember" ? m : (s === ".pfGroup" ? group : (s === ".pfGRow" ? r : null));
  return r;
}
const rows = [row("cline/a","3"),row("cline/b","3"),row("opencode/c","5")];
const document = {querySelectorAll:()=>[group]};
const $ = () => ({addEventListener:(kind,fn)=>{handler=fn}});
const read = new Function("document","$","rows", ` + string(code) + ` + "\nreturn pfReadModelGroups;")(document,$,rows);
rows[0].querySelector(".pfGPrio").value = "9";
if (handler) handler({target:rows[0].querySelector(".pfGPrio")});
assert.strictEqual(read().groups.group.platform_priorities.cline,9,"earlier priority edit was overwritten");
assert.strictEqual(rows[1].querySelector(".pfGPrio").value,"9","duplicate priority control did not synchronize");
assert.strictEqual(read().groups.group.platform_priorities.opencode,5);
rows[1].querySelector(".pfGPrio").value = "";
if (handler) handler({target:rows[1].querySelector(".pfGPrio")});
assert.ok(!("cline" in read().groups.group.platform_priorities),"clearing priority did not clear all duplicate controls");
`
	if out, err := exec.Command(node, "-e", program).CombinedOutput(); err != nil {
		t.Fatalf("priority behavior: %v\n%s", err, out)
	}
}

// The Platforms page owns model routing groups: a section above the platform
// cards where the operator edits the group name, its `client/model` members and
// one priority per member platform. Everything is local until Save & Apply,
// which goes through the same PATCH + reload path as the rest of the page.
func TestModelGroupUIEditorContract(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `id="pfGroups"`) {
		t.Error(`index.html has no model-group container (id="pfGroups")`)
	}
	if !strings.Contains(src, `id="pfBody"`) {
		t.Error(`index.html has no platform-card container (id="pfBody")`)
	}
	if strings.Index(src, `id="pfGroups"`) > strings.Index(src, `id="pfBody"`) {
		t.Error("the model-group editor must render above the platform cards")
	}

	render := poolStatsFuncBody(t, src, "renderPlatforms")
	for _, want := range []string{`pfModelGroupsHTML`, `model_groups`} {
		if !strings.Contains(render, want) {
			t.Errorf("renderPlatforms does not mention %q", want)
		}
	}

	editor := poolStatsFuncBody(t, src, "pfModelGroupsHTML")
	for _, want := range []string{`pfGAdd`, `pfModelGroupHTML`, `pfgList`} {
		if !strings.Contains(editor, want) {
			t.Errorf("pfModelGroupsHTML does not mention %q", want)
		}
	}

	group := poolStatsFuncBody(t, src, "pfModelGroupHTML")
	for _, want := range []string{
		`pfGroup`, `pfGName`, `pfGDel`, `pfGAddMember`, `pfModelGroupMemberHTML`,
	} {
		if !strings.Contains(group, want) {
			t.Errorf("pfModelGroupHTML does not mention %q", want)
		}
	}

	row := poolStatsFuncBody(t, src, "pfModelGroupMemberHTML")
	for _, want := range []string{`pfGMember`, `pfGPrio`, `pfGDelMember`} {
		if !strings.Contains(row, want) {
			t.Errorf("pfModelGroupMemberHTML does not mention %q", want)
		}
	}

	save := poolStatsFuncBody(t, src, "savePlatforms")
	for _, want := range []string{`pfReadModelGroups`, `model_groups`, `/panel/api/reload`} {
		if !strings.Contains(save, want) {
			t.Errorf("savePlatforms does not mention %q", want)
		}
	}

	if !strings.Contains(src, `$("#pfGroups").addEventListener`) {
		t.Error("the model-group editor has no event listener on #pfGroups")
	}
	for _, want := range []string{
		`ev.target.closest(".pfGAdd")`,
		`ev.target.closest(".pfGAddMember")`,
		`ev.target.closest(".pfGDelMember")`,
		`ev.target.closest(".pfGDel")`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("model-group editor has no event branch for %q", want)
		}
	}
}

// With dozens of routing groups the editor must stay navigable: the toolbar
// filters by group name or member, and each group can collapse to one summary
// row. A unique search hit opens automatically so the operator can edit it
// without another click.
func TestModelGroupUIHasSearchAndCollapseForLargeLists(t *testing.T) {
	src := poolStatsUISource(t)

	editor := poolStatsFuncBody(t, src, "pfModelGroupsHTML")
	for _, want := range []string{
		`pfGroupSearch`, `pfGExpandAll`, `pfGCollapseAll`,
		`pfgFilterNote`, `pfgEmpty`, `pfModelGroupHTML`,
	} {
		if !strings.Contains(editor, want) {
			t.Errorf("pfModelGroupsHTML does not render %q", want)
		}
	}

	group := poolStatsFuncBody(t, src, "pfModelGroupHTML")
	// pfgMeta 换成了成员节点链（pfChainHTML 渲染）：同一份成员摘要，从一行字
	// 升级成可读拓扑。钉的是「组头必须渲染成员摘要」这个契约，锚跟着形态更新。
	for _, want := range []string{
		`pfgToggle`, `aria-expanded`, `aria-controls`, `pfgBody`, `pfgTitle`, `pfChainHTML(summaryMembers)`,
	} {
		if !strings.Contains(group, want) {
			t.Errorf("pfModelGroupHTML does not render %q", want)
		}
	}

	// 节点链本体：Auto 成员必须有区别于普通成员的样式锚（通配一站 vs 具体站点），
	// 空组要有明确的「尚无成员」而不是一条空链。auto 标记是拼接出来的
	// 'pfn ' + (auto ? "auto" : "")，这里钉拼接式而不是成品字符串。
	chain := poolStatsFuncBody(t, src, "pfChainHTML")
	for _, want := range []string{
		`pfgchain`, `class="pfn ' + (auto ? "auto" : "")`, `尚无成员`, `pfnlink`,
	} {
		if !strings.Contains(chain, want) {
			t.Errorf("pfChainHTML does not render %q", want)
		}
	}

	filter := poolStatsFuncBody(t, src, "pfGroupFilter")
	for _, want := range []string{
		`pfGroupSearch`, `pfGroupMatchesQuery`, `pfGroupSetOpen`, `pfgFilterNote`, `pfgEmpty`,
	} {
		if !strings.Contains(filter, want) {
			t.Errorf("pfGroupFilter does not mention %q", want)
		}
	}

	listener := ""
	inputStart := strings.Index(src, `$("#pfGroups").addEventListener("input"`)
	clickStart := strings.Index(src, `$("#pfGroups").addEventListener("click"`)
	if inputStart >= 0 && clickStart > inputStart {
		rest := src[clickStart:]
		if end := strings.Index(rest, "\n});"); end >= 0 {
			listener = src[inputStart:clickStart] + rest[:end+4]
		}
	}
	for _, want := range []string{
		`ev.target.closest(".pfGroupSearch")`,
		`pfGroupFilter()`,
		`ev.target.closest(".pfGExpandAll")`,
		`ev.target.closest(".pfGCollapseAll")`,
		`ev.target.closest(".pfgToggle")`,
	} {
		if !strings.Contains(listener, want) {
			t.Errorf("model-group listener has no branch for %q", want)
		}
	}
}

func TestModelGroupNameEditorDoesNotStretchOnNarrowScreens(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, `  .pfgNameLabel { flex: none; width: 100%;`) {
		t.Error("the narrow-screen group-name label must not inherit the desktop 360px flex basis")
	}
	if !strings.Contains(src, `  .pfgNameLabel .pfGName { flex: none; width: 100%;`) {
		t.Error("the narrow-screen group-name input must not grow vertically")
	}
}

func TestModelGroupSearchMatchesNamesAndMembersCaseInsensitively(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	src := poolStatsUISource(t)
	match := poolStatsFuncBody(t, src, "pfGroupMatchesQuery") + "\n}"
	code, _ := json.Marshal(match)
	program := `
const assert = require("assert");
const match = new Function("return " + ` + string(code) + ` + ";")();
assert.strictEqual(match("", "DeepSeek-Flash", ["cline/a"]), true, "blank query should match everything");
assert.strictEqual(match("DEEP", "DeepSeek-Flash", ["cline/a"]), true, "group names should be case-insensitive");
assert.strictEqual(match("AUTO/FOO", "shared", ["Auto/foo"]), true, "member names should be case-insensitive");
assert.strictEqual(match("shared cline", "shared", ["cline/a"]), true, "space-separated terms should all match");
assert.strictEqual(match("shared missing", "shared", ["cline/a"]), false, "every search term must match");
assert.strictEqual(match("nope", "shared", ["cline/a"]), false);
`
	if out, err := exec.Command(node, "-e", program).CombinedOutput(); err != nil {
		t.Fatalf("model-group search behavior: %v\n%s", err, out)
	}
}

// Deleting a group or one of its priorities has to send an explicit null: the
// config PATCH is a recursive merge, so an omitted key would silently keep the
// old value on disk.
func TestModelGroupUISaveDeletesRemovedGroupsAndPriorities(t *testing.T) {
	src := poolStatsUISource(t)

	save := poolStatsFuncBody(t, src, "savePlatforms")
	for _, want := range []string{
		`groups[k] = null`,
		`prioPatch[k] = null`,
	} {
		if !strings.Contains(save, want) {
			t.Errorf("savePlatforms is missing delete semantics %q", want)
		}
	}

	read := poolStatsFuncBody(t, src, "pfReadModelGroups")
	for _, want := range []string{`pfGName`, `pfGMember`, `pfGPrio`, `Number.isInteger`, `groups`} {
		if !strings.Contains(read, want) {
			t.Errorf("pfReadModelGroups does not mention %q", want)
		}
	}
}

// The platform card's model rows carry the per-model priority.  It is the same
// number the route chain displays, so the row must offer an editable box, the
// reader must collect it, and a cleared box must delete the override rather
// than write 0.
func TestPlatformModelRowsCarryThePerModelPriority(t *testing.T) {
	src := poolStatsUISource(t)

	render := poolStatsFuncBody(t, src, "renderPlatforms")
	for _, want := range []string{`pfmPrio`, `model_priorities`, `data-inherited`, `pfmLbl`, `pfmBox`} {
		if !strings.Contains(render, want) {
			t.Errorf("renderPlatforms does not mention %q", want)
		}
	}

	read := poolStatsFuncBody(t, src, "pfReadModelPriorities")
	for _, want := range []string{`pfmPrio`, `dataset.model`} {
		if !strings.Contains(read, want) {
			t.Errorf("pfReadModelPriorities does not mention %q", want)
		}
	}

	save := poolStatsFuncBody(t, src, "savePlatforms")
	for _, want := range []string{
		`pfReadModelPriorities`,
		`modelPrioPatch`,
		`model_priorities`,
		`modelPrioPatch[model] = null`,
	} {
		if !strings.Contains(save, want) {
			t.Errorf("savePlatforms is missing %q", want)
		}
	}

	// The checkbox must be read through its own class: the row now holds a
	// second input, and a bare querySelector("input") would pick the wrong one.
	if strings.Contains(src, `m.querySelector("input")`) {
		t.Error("a model row still reads a bare querySelector(\"input\")")
	}
}

// Dragging in the route chain writes the per-model priority.  A blank box means
// inherit, so the drag must not touch the base, group or schedule priority.
func TestRouteDragWritesTheModelPriorityNotTheOtherRungs(t *testing.T) {
	src := poolStatsUISource(t)
	apply := poolStatsFuncBody(t, src, "applyRouteOrder")
	for _, want := range []string{`pfmPrio`, `pfmname`, `savePlatforms`} {
		if !strings.Contains(apply, want) {
			t.Errorf("applyRouteOrder does not mention %q", want)
		}
	}
	for _, unwanted := range []string{`.pfGPrio`, `.pfSchedPrio`} {
		if strings.Contains(apply, unwanted) {
			t.Errorf("applyRouteOrder still writes %s", unwanted)
		}
	}
}
