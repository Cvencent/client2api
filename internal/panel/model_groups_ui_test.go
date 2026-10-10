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
  p.closest = s => s === ".pfGPrio" ? p : (s === ".pfGroup" ? group : r);
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
