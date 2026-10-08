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
//   2. 「没有限流」和「模块不上报」真的被分开（键在不在是判据，不是长度）；
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

// TestModelColumnSeparatesNothingParkedFromNotReported 钉住判据：键在不在，
// 不是数组长不长。模块报了一个空数组 = 「现在没有模型被限流」；完全没有这个
// 键 = 「这个模块不上报模型级限流」。把两者画成同一个样子，操作员就分不清
// 「干净」和「看不到」。
func TestModelColumnSeparatesNothingParkedFromNotReported(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "acctModelSummary")
	if !strings.Contains(body, "if (!Array.isArray(raw)) return null;") {
		t.Error("acctModelSummary 没有把「键不存在」和「空数组」分开：判据必须是 Array.isArray")
	}
	if !strings.Contains(body, "const catalog = realmModels(n, realm);") {
		t.Error("acctModelSummary 没有按账号所在 realm 取模型目录")
	}

	cell := poolStatsFuncBody(t, src, "accModelsCell")
	if !strings.Contains(cell, "该平台不报模型级限流") {
		t.Error("不上报的模块没有说明文字，只剩一个没有解释的「—」")
	}

	// realmModels 要把 "cn:glm-5.3" 的 realm 前缀去掉，否则弹层里会前缀和账号
	// 的 realm 标签重复，也和 model_cooldowns 里的裸模型名对不上。
	rm := poolStatsFuncBody(t, src, "realmModels")
	if !strings.Contains(rm, `realm + ":"`) || !strings.Contains(rm, "id.slice(prefix.length)") {
		t.Error(`realmModels 没有按 "realm:" 前缀过滤并剥掉前缀`)
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
