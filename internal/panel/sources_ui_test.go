package panel

import (
	"encoding/json"
	"os/exec"
	"testing"
)

func TestSourcePoliciesPreserveUnavailableModelBlacklist(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	src := poolStatsUISource(t)
	code, _ := json.Marshal(poolStatsFuncBody(t, src, "pfReadDisabledModels") + "\n}")
	program := `
const assert=require("assert");
const read=new Function(` + string(code) + `+"\nreturn pfReadDisabledModels;")();
const model=(name,checked)=>({dataset:{model:name},querySelector:()=>({checked})});
let rows=[];
const p={dataset:{},querySelectorAll:()=>rows};
assert.deepStrictEqual(read(p,["blocked"]),["blocked"],"absent catalogue erased blacklist");
rows=[model("allowed",true),model("new-blocked",false)];
assert.deepStrictEqual(read(p,["blocked"]),["blocked","new-blocked"]);
p.dataset.clearBlacklist="1";rows=[model("blocked",true)];
assert.deepStrictEqual(read(p,["blocked"]),[],"explicit Allow All cannot clear saved blacklist");
`
	if out, err := exec.Command(node, "-e", program).CombinedOutput(); err != nil {
		t.Fatalf("blacklist behavior: %v\n%s", err, out)
	}
}
