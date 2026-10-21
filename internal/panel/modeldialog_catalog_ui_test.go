package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 账号弹层 / 模型列的目录来源回归测试。
//
// 两个页面读的是两份不同的目录，操作员看到的就是两套模型名：
//   - 「模型与档位」调 /panel/api/models，模块实时向上游取，trae 能列出
//     deepseek-v4.1-flash；
//   - 账号池的「模型」列和点开的弹层走 realmModels，而它原来只读
//     overview 的 Status.Models，那是 trae 的静态兜底目录，没有 V4.1 Flash。
//
// 于是同一个账号在「模型与档位」里有 V4.1 Flash，点开模型却没有。
// 面板必须以已经取到的实时目录为准，只在该模块没有返回目录时才退回
// Status.Models。
// ---------------------------------------------------------------------------

// TestModelDialogPrefersTheLiveCatalogueOverStatusModels 用 node 真跑一遍
// realmModels：实时目录存在时必须用它，缺失时才退回 overview 的目录。
func TestModelDialogPrefersTheLiveCatalogueOverStatusModels(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the catalogue source behavior needs it to run the inline function")
	}
	src := poolStatsUISource(t)
	block := poolStatsFuncBody(t, src, "realmModels") + "\n}"
	blockJSON, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("encoding realmModels for node: %v", err)
	}

	prog := `const fs = require("fs");
const out = process.argv[2];
try {
  const catalogForRealm = (models, realm, realms, custom) => (models || []).slice();
  const block = ` + string(blockJSON) + `;
  const cases = [
    {
      name: "live catalogue wins over the static status list",
      status: ["DeepSeek-V4-Flash", "glm-5.2"],
      live: ["deepseek-v4.1-flash", "glm-5.2"],
      realm: "", haveLive: true,
      want: ["deepseek-v4.1-flash", "glm-5.2"],
    },
    {
      name: "status list is the fallback when no live catalogue was fetched",
      status: ["DeepSeek-V4-Flash", "glm-5.2"],
      live: null,
      realm: "", haveLive: false,
      want: ["DeepSeek-V4-Flash", "glm-5.2"],
    },
    {
      name: "an empty live answer falls back instead of blanking the dialog",
      status: ["glm-5.2"],
      live: [],
      realm: "", haveLive: true,
      want: ["glm-5.2"],
    },
    {
      name: "another platform never borrows this one's live catalogue",
      status: ["glm-5.2"],
      live: ["kimi-k3"],
      realm: "", haveLive: false,
      want: ["glm-5.2"],
    },
  ];
  const got = cases.map(c => {
    const clientInfo = () => ({ name: "trae", models: c.status });
    const capsOf = () => ({});
    const MODEL_CATALOG = c.haveLive ? { trae: c.live } : {};
    const realmModels = new Function("clientInfo", "capsOf", "catalogForRealm", "MODEL_CATALOG",
      block + "\nreturn realmModels;")(clientInfo, capsOf, catalogForRealm, MODEL_CATALOG);
    return { name: c.name, want: c.want, got: realmModels("trae", c.realm) };
  });
  fs.writeFileSync(out, JSON.stringify({ ok: true, got }));
} catch (e) {
  fs.writeFileSync(out, JSON.stringify({ ok: false, error: String((e && e.message) || e) }));
}
`
	dir := t.TempDir()
	script := filepath.Join(dir, "realm-live.js")
	result := filepath.Join(dir, "realm-live.json")
	if err := os.WriteFile(script, []byte(prog), 0o600); err != nil {
		t.Fatalf("writing the node harness: %v", err)
	}
	if err := exec.Command(node, script, result).Run(); err != nil {
		t.Fatalf("node could not run realmModels: %v", err)
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("node wrote no result: %v", err)
	}
	var got struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Got   []struct {
			Name string   `json:"name"`
			Want []string `json:"want"`
			Got  []string `json:"got"`
		} `json:"got"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding the node result: %v\n%s", err, raw)
	}
	if !got.OK {
		t.Fatalf("the inline function threw: %s", got.Error)
	}
	if len(got.Got) != 4 {
		t.Fatalf("node returned %d cases, want 4", len(got.Got))
	}
	for _, c := range got.Got {
		if strings.Join(c.Got, ",") != strings.Join(c.Want, ",") {
			t.Errorf("%s: catalogue = %v, want %v", c.Name, c.Got, c.Want)
		}
	}
}

// TestModelDialogKeepsTheLiveCatalogueFilledFromModelsEndpoint 钉住「实时目录」
// 真的被两次 /panel/api/models 拉取填满：只改读取端而没人写入，弹层会永远
// 走回退分支，看起来像没修。
func TestModelDialogKeepsTheLiveCatalogueFilledFromModelsEndpoint(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, "MODEL_CATALOG = {}") {
		t.Fatal("index.html 没有实时模型目录这个状态，弹层只能退回 Status.Models")
	}
	// 写入实时目录的地方只有 applyModelCatalog 一处（两条取数路径共用它）。
	apply := poolStatsFuncBody(t, src, "applyModelCatalog")
	if !strings.Contains(apply, "MODEL_CATALOG[") {
		t.Error("applyModelCatalog 没有把 /panel/api/models 的结果写进实时目录，弹层仍会缺模型")
	}
	refresh := poolStatsFuncBody(t, src, "refreshModelCatalog")
	if !strings.Contains(refresh, "applyModelCatalog(") {
		t.Error("refreshModelCatalog 没有把拉到的新目录写进缓存，弹层永远看不到新模型")
	}
	if body := poolStatsFuncBody(t, src, "ensureModelNames"); !strings.Contains(body, "refreshModelCatalog(") {
		t.Error("ensureModelNames 没有复用共享的取数入口，弹层仍会缺模型")
	}
	if body := poolStatsFuncBody(t, src, "renderModels"); !strings.Contains(body, "applyModelCatalog(") {
		t.Error("renderModels 没有把这次 /panel/api/models 的结果写进实时目录，弹层仍会缺模型")
	}
}

// ---------------------------------------------------------------------------
// 实时目录的刷新节奏与失败降级。
//
// 上一条改动让账号弹层优先读 MODEL_CATALOG（/panel/api/models 的实时目录），
// 但那份缓存在页面上只写一次，之后会越来越旧：模块上架新模型时列表更新了，
// 账号弹层却还是老的。这里钉住三件事：
//   1. 账号页已有的 5s 轮询会顺带刷新实时目录，并按 TTL 节流、并发去重；
//   2. 刷新失败不清空已有目录，也不进入死循环；
//   3. 弹层在目录拉取失败时最多再试一次，然后退回 Status.Models 渲染，
//      不能因为一次网络失败就递归重试到页面卡死。
// ---------------------------------------------------------------------------

// TestModelCatalogRefreshIsThrottledAndDeduped 用 node 真跑 refreshModelCatalog：
// TTL 内的重复调用只打一次接口，force 能穿透节流，并发调用被合并，失败后仍可重试。
func TestModelCatalogRefreshIsThrottledAndDeduped(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the catalog refresh behavior needs it to run the inline function")
	}
	src := poolStatsUISource(t)
	block := poolStatsFuncBody(t, src, "refreshModelCatalog") + "\n}"
	blockJSON, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("encoding refreshModelCatalog for node: %v", err)
	}

	prog := `const fs = require("fs");
const out = process.argv[2];
(async () => {
  try {
    const block = ` + string(blockJSON) + `;
    const fn = new Function("api", "applyModelCatalog", "MODEL_CATALOG_TTL_MS",
      "MODEL_CATALOG_AT", "MODEL_CATALOG_INFLIGHT", "async " + block + "\nreturn refreshModelCatalog;");
    const make = (ok) => {
      let calls = 0;
      const api = async () => { calls++; return { ok: ok, status: 200, data: { clients: [{ name: "trae", models: [{ id: "deepseek-v4.1-flash" }] }] } }; };
      let applied = 0;
      const apply = () => { applied++; };
      const refresh = fn(api, apply, 30000, 0, null);
      return { refresh: refresh, calls: () => calls, applied: () => applied };
    };

    const a = make(true);
    await a.refresh(false);
    await a.refresh(false);
    const throttled = a.calls();
    await a.refresh(true);
    const forced = a.calls();

    const c = make(true);
    await Promise.all([c.refresh(false), c.refresh(false), c.refresh(false)]);
    const deduped = c.calls();

    const f = make(false);
    await f.refresh(false);
    await f.refresh(false);
    const failedRetries = f.calls();
    const failedApplied = f.applied();

    fs.writeFileSync(out, JSON.stringify({
      ok: true, throttled: throttled, forced: forced, deduped: deduped,
      failedRetries: failedRetries, failedApplied: failedApplied,
    }));
  } catch (e) {
    fs.writeFileSync(out, JSON.stringify({ ok: false, error: String((e && e.message) || e) }));
  }
})();
`
	dir := t.TempDir()
	script := filepath.Join(dir, "catalog-refresh.js")
	result := filepath.Join(dir, "catalog-refresh.json")
	if err := os.WriteFile(script, []byte(prog), 0o600); err != nil {
		t.Fatalf("writing the node harness: %v", err)
	}
	if err := exec.Command(node, script, result).Run(); err != nil {
		t.Fatalf("node could not run refreshModelCatalog: %v", err)
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("node wrote no result: %v", err)
	}
	var got struct {
		OK            bool   `json:"ok"`
		Error         string `json:"error"`
		Throttled     int    `json:"throttled"`
		Forced        int    `json:"forced"`
		Deduped       int    `json:"deduped"`
		FailedRetries int    `json:"failedRetries"`
		FailedApplied int    `json:"failedApplied"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding the node result: %v\n%s", err, raw)
	}
	if !got.OK {
		t.Fatalf("refreshModelCatalog threw: %s", got.Error)
	}
	if got.Throttled != 1 {
		t.Errorf("TTL 内第二次刷新打了 %d 次接口，want 1：账号页每 5s 轮询会因此每轮都重取", got.Throttled)
	}
	if got.Forced != 2 {
		t.Errorf("force=true 后共打了 %d 次接口，want 2：手动刷新穿不过节流", got.Forced)
	}
	if got.Deduped != 1 {
		t.Errorf("三个并发刷新打了 %d 次接口，want 1：同一时刻会重复拉取", got.Deduped)
	}
	if got.FailedRetries != 2 {
		t.Errorf("失败后重试共打了 %d 次接口，want 2：失败被当成成功节流住，之后再也刷不出来", got.FailedRetries)
	}
	if got.FailedApplied != 0 {
		t.Errorf("失败的刷新写了 %d 次目录，want 0：失败不能被当成新数据", got.FailedApplied)
	}
}

// TestAccountPollRefreshesTheModelCatalog 钉住接线：账号页那 5s 轮询要把实时
// 目录一起刷新，否则弹层会一直用打开页面那一刻的旧列表。
func TestAccountPollRefreshesTheModelCatalog(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		"MODEL_CATALOG_TTL_MS",
		"MODEL_CATALOG_INFLIGHT",
		"function refreshModelCatalog(",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("index.html 缺 %q：实时目录没有可复用的刷新入口", want)
		}
	}
	vis := poolStatsFuncBody(t, src, "refreshVisible")
	if !strings.Contains(vis, "refreshModelCatalog(") {
		t.Error("账号页轮询没有刷新实时目录，弹层会一直停留在打开页面时的模型列表")
	}
	// 不能被 wait 住：/panel/api/models 会 fan-out 到所有模块（预算 8s），
	// await 它会把账号表的刷新一起拖慢。这里要求用 .catch 这种发射后不管的写法。
	if strings.Contains(vis, "await refreshModelCatalog(") {
		t.Error("账号页轮询在等实时目录刷新完成：模型目录慢的时候账号表会跟着卡")
	}
}

// TestModelDialogStopsRetryingWhenTheCatalogFetchFails 钉住失败降级：目录拉不
// 回来时，弹层最多再试一次就必须照旧渲染（退回 Status.Models），不能无限递归。
func TestModelDialogStopsRetryingWhenTheCatalogFetchFails(t *testing.T) {
	src := poolStatsUISource(t)
	dialog := poolStatsFuncBody(t, src, "openModelsDialog")
	if !strings.Contains(dialog, "triedCatalog") {
		t.Error("openModelsDialog 没有记录「已经试过拉目录」：拉取失败会一直递归重试")
	}
	if strings.Contains(dialog, ".then(() => openModelsDialog(n, ids))") {
		t.Error("openModelsDialog 仍是无条件递归：目录接口一失败页面就会卡在重试里")
	}
	if !strings.Contains(dialog, "openModelsDialog(n, ids, true)") {
		t.Error("openModelsDialog 没有把「已试过」传给下一次调用，重试没有上界")
	}
}
