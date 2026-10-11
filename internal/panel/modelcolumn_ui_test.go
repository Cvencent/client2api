package panel

import (
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 账号池「模型」列的回归测试。
//
// 这一列比别的列多一层跨包契约：模块把「哪个模型被限流」写成结构化条目
// （core.ModelPark）塞进 Status().Accounts[].Extra["model_cooldowns"]，面板
// 再把它和 overview 的模型目录拼成「可用 N · 限流 M」。两边任何一处对不上，
// 这一列就能静默地永远显示成「—」，而 Go 编译和浏览器都不会报错。所以这里
// 钉住三件事：
//   1. 界面读的键 == core.ModelPark 的 json tag；
//   2. 模块没有上报逐模型状态时，账号模型列会回退到平台模型目录，而不是留白；
//   3. 表头、行模板、组头、弹层、事件接线都还在。
// ---------------------------------------------------------------------------

func TestModelColumnReadsTheKeysModelParkSends(t *testing.T) {
	b, err := os.ReadFile("../core/core.go")
	if err != nil {
		t.Skipf("cannot read ../core/core.go: %v", err)
	}
	goSrc := string(b)
	// core.ModelPark 必须带着面板读的每一个键。
	for _, k := range []string{"model", "kind", "until", "reset_at"} {
		if !strings.Contains(goSrc, "json:\""+k) {
			t.Errorf("core.ModelPark 没有 json tag %q", k)
		}
	}

	src := poolStatsUISource(t)
	for _, want := range []string{
		`ex.model_cooldowns`,                         // 状态 extra 里读的就是这个键
		`p.model`, `p.kind`, `p.until`, `p.reset_at`, // 结构化条目的字段名
	} {
		if !strings.Contains(src, want) {
			t.Errorf("界面里找不到 %q：模型列没有读到模块发过来的字段", want)
		}
	}
}

// TestModelColumnFallsBackToTheAccountCatalog 钉住这列的补全口径：模块上报了
// model_cooldowns 就叠加限流/不支持；没上报这个键时，不再把整格画成「—」，
// 而是用账号自己的模型清单或平台目录显示「可用 N」。
//
// 这条守卫会抓住的破坏：把 acctModelSummary 改回遇到缺失键就 return null，
// 或让它忽略账号 fields.models / realm / provider 前缀，导致某类账号又只剩
// 一个没有说明的「—」。
func TestModelColumnFallsBackToTheAccountCatalog(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "acctModelSummary")
	for _, want := range []string{
		`const raw = ex ? ex.model_cooldowns : undefined;`,
		`const parksReported = Array.isArray(raw);`,
		`const catalog = accountModelCatalog(n, a);`,
		`parksReported: parksReported,`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("acctModelSummary 缺少 %q：没有上报限流的账号不会回退到模型目录", want)
		}
	}
	if strings.Contains(body, "if (!Array.isArray(raw)) return null;") {
		t.Error("acctModelSummary 仍在缺失 model_cooldowns 时直接返回 null：模型列会继续留白")
	}

	cat := poolStatsFuncBody(t, src, "accountModelCatalog")
	for _, want := range []string{
		`fields.models`,
		`realmModels(n, realm)`,
		`id + "/"`,
		`slice(id.length + 1)`,
	} {
		if !strings.Contains(cat, want) {
			t.Errorf("accountModelCatalog 缺少 %q：账号级模型目录没有补齐", want)
		}
	}

	cell := poolStatsFuncBody(t, src, "accModelsCell")
	if !strings.Contains(cell, "按平台目录全部可用") {
		t.Error("不回退到平台目录，模型列仍会只显示「—」")
	}
	if strings.Contains(cell, "该平台不报模型级限流") {
		t.Error("accModelsCell 还在把可回退到平台目录的账号画成「不上报」")
	}

	// realmModels 要把 "cn:glm-5.3" 的 realm 前缀去掉，否则弹层里会前缀和账号
	// 的 realm 标签重复，也和 model_cooldowns 里的裸模型名对不上。它只按模块
	// capabilities.realms 里声明过的 code 认前缀，行为用例在
	// TestModelColumnStripsOnlyDeclaredRealmPrefixes。
	rm := poolStatsFuncBody(t, src, "realmModels")
	if !strings.Contains(rm, "catalogForRealm(") || !strings.Contains(rm, "caps.realms") {
		t.Error("realmModels 没有把「声明过的 realm 列表」交给 catalogForRealm")
	}
}

// TestModelColumnIsWiredEndToEnd 钉住这条链路从表头到弹层都在：表头一格、
// 行模板用 accModelsCell、组头也留一格（否则多通道账号的列会整体错位）、
// 列表里点得到、弹层和 Esc 都接上了。
func TestModelColumnIsWiredEndToEnd(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `>模型</th>`) {
		t.Error("账号池表头没有「模型」列")
	}
	if !strings.Contains(src, `'<td class="c-models">' + accModelsCell(CUR, a) + '</td>'`) {
		t.Error("行模板没有用 accModelsCell 渲染模型列")
	}
	// 组头是固定格之一；少了它，多通道账号下面的每一格都会右移一位。
	if !strings.Contains(src, `accGroupModelsCell(CUR, g.rows)`) {
		t.Error("组头没有给模型列留格，多通道账号会整体错位")
	}

	// 列表里点得开：模型 chip 必须在通用 act() 之前被拦下来。
	if !strings.Contains(src, `button[data-do="models"]`) {
		t.Error("模型 chip 没有接线，点了没有任何反应")
	}
	if !strings.Contains(src, "openModelsDialog(CUR, ids)") {
		t.Error("点模型 chip 没有真的打开弹层")
	}
	// 弹层本体和 Esc 收尾。
	if !strings.Contains(src, `id="modelVeil"`) || !strings.Contains(src, `id="modelBody"`) {
		t.Error("模型弹层的 HTML 不完整（缺 modelVeil / modelBody）")
	}
	if !strings.Contains(src, `if (v.id === "modelVeil") { closeModelsDialog(); return true; }`) {
		t.Error("Esc 关不掉模型弹层")
	}
}

// TestModelDialogShowsFriendlyNames keeps the Qoder account-pool dialog
// readable: the module already publishes display_name in Extra, so the cell
// shows that name while the title stays the wire model id the gateway expects.
func TestModelDialogShowsFriendlyNames(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{`MODEL_NAMES`, `display_name`, `function modelDisplayName(client, id)`} {
		if !strings.Contains(src, want) {
			t.Errorf("model dialog is missing %q", want)
		}
	}
	cell := poolStatsFuncBody(t, src, "modelCellHTML")
	if !strings.Contains(cell, `title="' + esc(m) + '"`) || !strings.Contains(cell, `esc(shown)`) {
		t.Error("modelCellHTML does not keep the raw id as title while showing the friendly name")
	}
	dialog := poolStatsFuncBody(t, src, "openModelsDialog")
	if !strings.Contains(dialog, "modelCellHTML(n, m, sum.parks[m])") {
		t.Error("openModelsDialog does not pass the client into modelCellHTML")
	}
	render := poolStatsFuncBody(t, src, "renderModels")
	if !strings.Contains(render, `MODEL_NAMES[c.name + "/" + m.id] = nm`) {
		t.Error("renderModels does not cache display names for the account-pool dialog")
	}
}
