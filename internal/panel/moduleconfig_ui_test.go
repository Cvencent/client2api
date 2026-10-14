package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 配置页的「模块配置」表单。
//
// 背景：clients.<模块> 是模块自己解析的 JSON，面板过去只能甩裸 textarea。
// 现在按后端的 module_schema 画带中文标签的控件，没覆盖的键留在「其它键」。
// 这里钉两件事：
//   1. 渲染与保存真的走了这套控件，而不是还留着旧的裸 JSON 路径；
//   2. 纯函数的行为正确 —— 空控件=删键、只回写变化的键、类型不对的值不硬塞。
//      node 只是可选测试依赖，没有就跳过第 2 条。
// ---------------------------------------------------------------------------

// modHelperSource 截取从 modDisplayValue 到 modCardHTML 之间的那批纯辅助函数。
// 它们不依赖 DOM / esc / CFG，可以直接扔进 node 跑；modCardHTML 开始就要 esc 了，
// 所以停在它前面。
func modHelperSource(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "function modDisplayValue(")
	if start < 0 {
		t.Fatal("index.html has no function modDisplayValue")
	}
	rest := src[start:]
	end := strings.Index(rest, "function modCardHTML(")
	if end < 0 {
		t.Fatal("modHelperSource has no end marker (modCardHTML)")
	}
	return rest[:end]
}

// TestModuleConfigUsesSchemaDrivenControls 是静态接线断言：平台页的每张平台卡片里
// 要嵌一张模块配置卡（modCardHTML + module_schema），保存要走 modBuildValue /
// modDiffPatch，旧的 .cfgmod 裸 JSON 选择器必须消失；全局配置页不再画模块卡片。
func TestModuleConfigUsesSchemaDrivenControls(t *testing.T) {
	src := poolStatsUISource(t)
	render := poolStatsFuncBody(t, src, "renderPlatforms")
	if !strings.Contains(render, "modCardHTML(") {
		t.Error("renderPlatforms 没有走 modCardHTML，模块配置还是裸 JSON")
	}
	if !strings.Contains(render, "module_schema") {
		t.Error("renderPlatforms 没有读后端的 module_schema")
	}
	save := poolStatsFuncBody(t, src, "savePlatforms")
	for _, want := range []string{"modBuildValue(", "modDiffPatch(", `.cfgmodcard`} {
		if !strings.Contains(save, want) {
			t.Errorf("savePlatforms 没有用 %q", want)
		}
	}
	if strings.Contains(save, `querySelectorAll(".cfgmod")`) {
		t.Error("savePlatforms 还在读旧的 .cfgmod 裸 JSON 文本框")
	}
	// 模块配置已经整体搬去「平台配置」页；全局配置页再画一遍会形成两个入口，
	// 而且那边的 CFG.baseline 剪枝不认 clients.<name> 的逐键 diff。
	renderCfg := poolStatsFuncBody(t, src, "renderConfig")
	if strings.Contains(renderCfg, "modCardHTML(") {
		t.Error("renderConfig 又渲染了模块卡片：clients.<模块> 只在「平台配置」页编辑")
	}
	saveCfg := poolStatsFuncBody(t, src, "saveConfig")
	if strings.Contains(saveCfg, ".cfgmodcard") || strings.Contains(saveCfg, "patch.clients") {
		t.Error("saveConfig 又在保存模块配置：它已经跟着卡片搬去「平台配置」页了")
	}
	// 生成的控件要带稳定 id，平台页的草稿保存才不依赖 DOM 节点的身份。
	if !strings.Contains(src, `modDomID(mod, o.key)`) || !strings.Contains(src, `modDomID(name, "__extra")`) {
		t.Error("模块配置控件没有稳定 id：capturePlatformDraft 的刷新保稿会丢编辑")
	}
	// 卡片嵌在平台卡片里，标题不能再用模块名（外面已经有一个）；恢复默认的口径
	// 也要写在卡片里，否则操作员不知道清空控件等于回到内置值。
	for _, want := range []string{"'模块配置'", "清空所有控件并把「其它键」留空或写成"} {
		if !strings.Contains(src, want) {
			t.Errorf("模块配置卡片缺少 %q", want)
		}
	}
	modHelperSource(t, src) // 辅助函数必须都在，且可截取
}

// TestModuleConfigHelpersRoundTrip 用 node 把辅助函数跑一遍，证明的是行为：
// 空控件删键、表单覆盖「其它键」、只回写变化的键、类型不对的值退回 JSON。
func TestModuleConfigHelpersRoundTrip(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the module-config helpers need it to run")
	}
	block := modHelperSource(t, poolStatsUISource(t))
	script := "const REDACTED = \"<redacted>\";\n" + block + "\n" + `
const eq = (a, b) => JSON.stringify(a) === JSON.stringify(b);
const fail = m => { console.log("FAIL: " + m); process.exit(1); };

if (!eq(modOptionValue("bool", ""), {set: false})) fail("空的布尔格应表示删键");
if (!eq(modOptionValue("bool", "true"), {set: true, value: true})) fail("布尔 true 没解析");
if (!eq(modOptionValue("bool", "false"), {set: true, value: false})) fail("布尔 false 没解析");
if (!eq(modOptionValue("number", "5"), {set: true, value: 5})) fail("数字没解析");
if (!modOptionValue("number", "abc").error) fail("非数字应报错");
if (!eq(modOptionValue("list", "a, b ,,c"), {set: true, value: ["a", "b", "c"]})) fail("列表没按逗号切");
if (!eq(modOptionValue("text", "<redacted>"), {set: true, value: "<redacted>"})) fail("凭据占位符必须原样回传");

// 表单覆盖「其它键」：两边都有 tls_profile 时以表单为准。
let b = modBuildValue(
  [{key: "tls_profile", kind: "choice", raw: "chrome"}, {key: "free_only", kind: "bool", raw: ""}],
  '{"captcha_browser":true,"tls_profile":"firefox","free_only":true}');
if (b.error) fail("合法的输入被判成错误：" + b.error);
if (!eq(b.value, {captcha_browser: true, tls_profile: "chrome"})) fail("表单没有覆盖其它键，或空控件没有删键：" + JSON.stringify(b.value));

// 「其它键」必须是对象。
if (!modBuildValue([], "not json").error) fail("非法 JSON 应报错");
if (!modBuildValue([], "[1]").error) fail("数组不是合法的其它键");
if (!eq(modBuildValue([], "").value, {})) fail("空的其它键应得到空对象");
if (!eq(modBuildValue([], "{}").value, {})) fail("{} 的其它键应得到空对象");

// 只回写变化的键：删掉的发 null，没动的键不出现。
if (!eq(modDiffPatch({a: 1, b: 2}, {a: 1}), {b: null})) fail("删掉的键没有发 null");
if (!eq(modDiffPatch({a: 1}, {a: 2, c: 3}), {a: 2, c: 3})) fail("变化的键没有全发");
if (!eq(modDiffPatch({a: 1}, {a: 1}), {})) fail("没动的键不该回写");
if (!eq(modDiffPatch({a: {x: 1}}, {a: {x: 1}}), {})) fail("相等的对象不该回写");
if (!eq(modDiffPatch(null, {a: 1}), {a: 1})) fail("没有基线时应全发");

// 类型对不上的值退回「其它键」，不能被硬塞进控件。
if (modFormFits("list", [1, 2])) fail("数字数组不该当成字符串列表");
if (!modFormFits("list", ["a"])) fail("字符串数组应能用列表控件");
if (modFormFits("bool", "yes")) fail("字符串不该当成布尔");
if (modFormFits("number", "5")) fail("字符串不该当成数字");
if (!modFormFits("bool", true)) fail("布尔应能用布尔控件");
if (modFormFits("choice", "legacy", [{value:"chrome"}, {value:"firefox"}])) fail("下拉里没有的旧值不能被悄悄当成默认值");
if (!modFormFits("choice", "chrome", [{value:"chrome"}, {value:"firefox"}])) fail("下拉里已有的值应能用控件");
if (!modFormFits("text", undefined)) fail("没配过的键永远可以编辑");

console.log("ok");
`
	dir := t.TempDir()
	js := filepath.Join(dir, "modhelpers.js")
	if err := os.WriteFile(js, []byte(script), 0o600); err != nil {
		t.Fatalf("writing the node script: %v", err)
	}
	out, err := exec.Command(node, js).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("node output = %s, want ok", out)
	}
}
