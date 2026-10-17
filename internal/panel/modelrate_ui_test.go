package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAccountModelDialogRendersRateTags runs the inline renderer so a paid
// price cannot hide an explicit free marker, a stale list price cannot hide the
// current promotion, and an unknown rate does not grow an invented tag.
func TestAccountModelDialogRendersRateTags(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the model-rate renderer needs it")
	}

	src := poolStatsUISource(t)
	var bundle strings.Builder
	for _, name := range []string{
		"rateNumber",
		"rateIsFree",
		"fmtMultiplier",
		"rateListPrice",
		"rateEffectivePrice",
		"rateFromDisplayName",
		"modelRateTagHTML",
	} {
		bundle.WriteString(poolStatsFuncBody(t, src, name))
		bundle.WriteString("\n}\n")
	}

	cases := []struct {
		Name  string         `json:"name"`
		Extra map[string]any `json:"extra"`
		Want  string         `json:"want"`
	}{
		{"explicit free wins", map[string]any{"free": true, "credits": 2}, `<span class="tag ok model-rate" title="免费模型">免费</span>`},
		{"zero multiplier is free", map[string]any{"display_multiplier": 0}, `<span class="tag ok model-rate" title="免费模型">免费</span>`},
		{"list multiplier", map[string]any{"credits": 2}, `<span class="tag mute model-rate" title="模型倍率">x2</span>`},
		{"current promotion", map[string]any{"credits": 2, "promo_credits": 0.8}, `<span class="tag mute model-rate" title="当前生效倍率">x0.8</span>`},
		{"display-name multiplier", map[string]any{"display_name": "DeepSeek V4 · x3"}, `<span class="tag mute model-rate" title="模型倍率">x3</span>`},
		{"unknown rate", map[string]any{"display_name": "DeepSeek V4"}, ""},
	}

	dir := t.TempDir()
	casesPath := filepath.Join(dir, "cases.json")
	resultPath := filepath.Join(dir, "result.json")
	rawCases, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("encoding cases: %v", err)
	}
	if err := os.WriteFile(casesPath, rawCases, 0o600); err != nil {
		t.Fatalf("writing cases: %v", err)
	}

	scriptPath := filepath.Join(dir, "model-rate.js")
	script := `
const fs = require("fs");
function esc(v) {
  return String(v == null ? "" : v)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}
` + bundle.String() + `
const cases = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));
const got = cases.map(c => ({name: c.name, got: modelRateTagHTML(c.extra)}));
fs.writeFileSync(process.argv[3], JSON.stringify(got));
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatalf("writing node harness: %v", err)
	}
	if out, err := exec.Command(node, scriptPath, casesPath, resultPath).CombinedOutput(); err != nil {
		t.Fatalf("node could not run the model-rate renderer: %v\n%s", err, out)
	}
	rawResult, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading node result: %v", err)
	}
	var got []struct {
		Name string `json:"name"`
		Got  string `json:"got"`
	}
	if err := json.Unmarshal(rawResult, &got); err != nil {
		t.Fatalf("decoding node result: %v\n%s", err, rawResult)
	}
	if len(got) != len(cases) {
		t.Fatalf("node returned %d cases, want %d", len(got), len(cases))
	}
	for i, tc := range cases {
		if got[i].Name != tc.Name {
			t.Fatalf("case %d name = %q, want %q", i, got[i].Name, tc.Name)
		}
		if got[i].Got != tc.Want {
			t.Errorf("%s:\n got %s\nwant %s", tc.Name, got[i].Got, tc.Want)
		}
	}
}

// TestAccountModelDialogEmbedsRateTagsInModelCells protects the wiring between
// the cached catalogue metadata and both dialog render paths.  A renderer can
// be perfect and still be useless if modelCellHTML never asks it for a tag.
func TestAccountModelDialogEmbedsRateTagsInModelCells(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the model-cell renderer needs it")
	}

	src := poolStatsUISource(t)
	var bundle strings.Builder
	for _, name := range []string{
		"modelDisplayName",
		"modelExtra",
		"rateNumber",
		"rateIsFree",
		"fmtMultiplier",
		"rateListPrice",
		"rateEffectivePrice",
		"rateFromDisplayName",
		"modelRateTagHTML",
		"modelCellHTML",
	} {
		bundle.WriteString(poolStatsFuncBody(t, src, name))
		bundle.WriteString("\n}\n")
	}

	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.json")
	scriptPath := filepath.Join(dir, "model-cell.js")
	script := `
const fs = require("fs");
const MODEL_NAMES = {
  "trae/paid": "Paid Model",
  "trae/free": "Free Model",
  "trae/plain": "Plain Model"
};
const MODEL_META = {
  "trae/paid": {credits: 2},
  "trae/free": {free: true},
  "trae/plain": {}
};
function esc(v) {
  return String(v == null ? "" : v)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}
function parkStateHTML() { return ""; }
` + bundle.String() + `
const got = {
  paidEditable: modelCellHTML("trae", "paid", null, true, false),
  freeReadonly: modelCellHTML("trae", "free", null, false, false),
  unknownReadonly: modelCellHTML("trae", "plain", null, false, false)
};
fs.writeFileSync(process.argv[2], JSON.stringify(got));
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatalf("writing node harness: %v", err)
	}
	if out, err := exec.Command(node, scriptPath, resultPath).CombinedOutput(); err != nil {
		t.Fatalf("node could not run modelCellHTML: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading node result: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding node result: %v\n%s", err, raw)
	}
	for _, tc := range []struct {
		name string
		want []string
		deny string
	}{
		{"paidEditable", []string{"Paid Model", `title="模型倍率">x2</span>`, `class="mparkBox" checked`}, ""},
		{"freeReadonly", []string{"Free Model", `title="免费模型">免费</span>`}, ""},
		{"unknownReadonly", []string{"Plain Model"}, "model-rate"},
	} {
		for _, want := range tc.want {
			if !strings.Contains(got[tc.name], want) {
				t.Errorf("%s = %s\nmissing %s", tc.name, got[tc.name], want)
			}
		}
		if tc.deny != "" && strings.Contains(got[tc.name], tc.deny) {
			t.Errorf("%s = %s\nunexpected %s", tc.name, got[tc.name], tc.deny)
		}
	}
}

// TestAccountModelDialogCachesCatalogueRateMetadata keeps the account dialog
// connected to the live catalogue payload.  The dialog opens from the accounts
// view, so it cannot assume the model page has already been visited.
func TestAccountModelDialogCachesCatalogueRateMetadata(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the catalogue cache behavior needs it")
	}

	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "rememberModelMeta")
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.json")
	scriptPath := filepath.Join(dir, "model-cache.js")
	script := `
const fs = require("fs");
let MODEL_NAMES = {};
let MODEL_META = {};
` + body + `
}
rememberModelMeta("trae", {id: "paid", extra: {display_name: "Paid Model", credits: 2}});
rememberModelMeta("trae", {id: "plain"});
fs.writeFileSync(process.argv[2], JSON.stringify({
  paidName: MODEL_NAMES["trae/paid"] || "",
  paidRate: MODEL_META["trae/paid"] && MODEL_META["trae/paid"].credits,
  plainName: MODEL_NAMES["trae/plain"] || "",
  plainMeta: MODEL_META["trae/plain"]
}));
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatalf("writing node harness: %v", err)
	}
	if out, err := exec.Command(node, scriptPath, resultPath).CombinedOutput(); err != nil {
		t.Fatalf("node could not run rememberModelMeta: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading node result: %v", err)
	}
	var got struct {
		PaidName  string `json:"paidName"`
		PaidRate  int    `json:"paidRate"`
		PlainName string `json:"plainName"`
		PlainMeta any    `json:"plainMeta"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding node result: %v\n%s", err, raw)
	}
	if got.PaidName != "Paid Model" || got.PaidRate != 2 {
		t.Errorf("paid cache = name %q rate %d, want Paid Model / 2", got.PaidName, got.PaidRate)
	}
	if got.PlainName != "" || got.PlainMeta == nil {
		t.Errorf("plain cache = name %q meta %#v, want empty name and an empty metadata object", got.PlainName, got.PlainMeta)
	}
}
