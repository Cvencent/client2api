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
// 行级签到动作的通道收窄。
//
// 背景：签到动作是客户端级的（core.CheckinProvider.CheckinActions）。zcode 的
// 「领取活动套餐」走计划账单接口，只认计划 JWT。模块用 CheckinAction.Channels
// 声明它需要哪些通道，面板在 accRowHTML 里按「账号」收窄，而不是按单条凭据行：
// 同一个账号的 JWT 和它的编码计划 API key 被模块按 identity 收成一组，组里只要
// 有 jwt，编码计划 API key 那一行也能点——点下去由模块的 claimChannelFor 解析出
// 真正的 JWT 兄弟通道。只有孤立的 API key 账号（组里根本没有 jwt）才拿不到动作，
// 免得长出一个点了必然失败的领取按钮。
//
// 这里钉三件事：
//   1. accRowHTML 真的走了这个过滤器，而不是直接铺 acts；
//   2. 过滤器把「整组」当作用域（renderAccounts 把 g.rows 传了进来）；
//   3. 过滤器本身的行为正确——用 node 把它拉出来实际跑一遍。
//
// node 只是可选的测试依赖：没有 node 就跳过第 3 条。
// ---------------------------------------------------------------------------

// checkinActionsForSource 截取 index.html 里那个纯粹的行级动作过滤器。它以行首
// 的 "}" 结束，所以和 poolStatsFuncBody 一样截到下一个 "\n}" 为止。它必须是自足
// 的（不引用 CUR / esc 等外部名字），否则下面的 node 用例拉出来就跑不起来。
func checkinActionsForSource(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "function checkinActionsFor(")
	if start < 0 {
		t.Fatal("index.html has no function checkinActionsFor")
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatal("checkinActionsFor looks unterminated")
	}
	return rest[:end+2]
}

// TestAccountRowScopesCheckinActionsToTheCredentialChannel 是静态接线断言：
// accRowHTML 必须先把客户端级动作按「这一行所属账号」的通道收窄，再去渲染按钮。
func TestAccountRowScopesCheckinActionsToTheCredentialChannel(t *testing.T) {
	src := poolStatsUISource(t)
	row := poolStatsFuncBody(t, src, "accRowHTML")
	if !strings.Contains(row, "checkinActionsFor(a, acts, group)") {
		t.Fatal("accRowHTML 没有按账号的通道收窄签到动作：编码计划 API key 行也会长出领取按钮")
	}
	// 收窄之后就不该再直接铺客户端级的那份 acts 了。
	if strings.Contains(row, "acts.forEach") || strings.Contains(row, "acts[0]") {
		t.Fatal("accRowHTML 还在直接渲染客户端级的 acts，没有全走 checkinActionsFor")
	}
	// 组级作用域必须真的被传进来：renderAccounts 调 accRowHTML 时带上 g.rows。
	if !strings.Contains(src, `accRowHTML(g.rows[0], caps, acts, showBal, false, false, "", g.rows)`) {
		t.Fatal("单通道行没有把自己的组传给 accRowHTML")
	}
	if !strings.Contains(src, "accRowHTML(a, caps, acts, showBal, true, open, g.key, g.rows)") {
		t.Fatal("多通道行没有把自己的组传给 accRowHTML")
	}
	checkinActionsForSource(t, src) // 过滤器必须在，且可截取
}

// TestCheckinActionsForNarrowsByAccountChannels 用 node 把过滤器拉出来实际跑一遍。
// 它证明的是行为而不是文字：账号组里有 jwt，编码计划 API key 行也能拿到领取动作；
// 组里没有 jwt 的孤立 API key 行拿不到；jwt 行本身照旧拿到；没声明通道的动作照旧
// 每一行都出现。没有 node 就跳过——面板本身不需要 node。
func TestCheckinActionsForNarrowsByAccountChannels(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the row-filter behavior needs it to run the inline function")
	}
	block := checkinActionsForSource(t, poolStatsUISource(t))
	blockJSON, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("encoding the filter for node: %v", err)
	}

	// cases：同一账号组里的通道集合 -> 这一行应该拿到的动作 id。group 是整组。
	prog := `const fs = require("fs");
const out = process.argv[2];
try {
  const block = ` + string(blockJSON) + `;
  const fn = new Function(block + "\nreturn checkinActionsFor;")();
  const acts = [
    { id: "claim", label: "领取活动套餐", channels: ["jwt"] },
    { id: "daily", label: "每日签到" },
  ];
  const keyRow = { id: "key", fields: { kind: "api-key" } };
  const jwtRow = { id: "jwt", fields: { kind: "jwt" } };
  const rows = [
    { name: "api-key row in an account that also holds a jwt", row: keyRow, group: [keyRow, jwtRow], want: ["claim", "daily"] },
    { name: "jwt row in a mixed account", row: jwtRow, group: [keyRow, jwtRow], want: ["claim", "daily"] },
    { name: "isolated api-key row (no jwt sibling)", row: keyRow, group: [keyRow], want: ["daily"] },
    { name: "isolated api-key row, no group passed", row: keyRow, want: ["daily"] },
    { name: "jwt row alone", row: jwtRow, group: [jwtRow], want: ["claim", "daily"] },
    { name: "kind case-insensitive", row: { id: "x", fields: { kind: "JWT" } }, group: [{ id: "x", fields: { kind: "JWT" } }], want: ["claim", "daily"] },
    { name: "no kind reported", row: { id: "x", fields: {} }, want: ["daily"] },
    { name: "no fields at all", row: { id: "x" }, want: ["daily"] },
  ];
  const got = rows.map(r => ({ name: r.name, want: r.want, ids: fn(r.row, acts, r.group).map(x => x.id) }));
  fs.writeFileSync(out, JSON.stringify({ ok: true, got, actsLeft: acts.length }));
} catch (e) {
  fs.writeFileSync(out, JSON.stringify({ ok: false, error: String((e && e.message) || e) }));
}
`
	dir := t.TempDir()
	script := filepath.Join(dir, "filter.js")
	result := filepath.Join(dir, "filter.json")
	if err := os.WriteFile(script, []byte(prog), 0o600); err != nil {
		t.Fatalf("writing the node harness: %v", err)
	}
	if err := exec.Command(node, script, result).Run(); err != nil {
		t.Fatalf("node could not run the inline filter: %v", err)
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("node wrote no result: %v", err)
	}
	var got struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		ActsLeft int    `json:"actsLeft"`
		Got      []struct {
			Name string   `json:"name"`
			Want []string `json:"want"`
			IDs  []string `json:"ids"`
		} `json:"got"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding the node result: %v\n%s", err, raw)
	}
	if !got.OK {
		t.Fatalf("the inline filter threw: %s", got.Error)
	}
	if len(got.Got) != 8 {
		t.Fatalf("node returned %d cases, want 8", len(got.Got))
	}
	for _, c := range got.Got {
		if strings.Join(c.IDs, ",") != strings.Join(c.Want, ",") {
			t.Errorf("%s: actions = %v, want %v", c.Name, c.IDs, c.Want)
		}
	}
	// 过滤是只读的：原动作列表不能被就地改动。
	if got.ActsLeft != 2 {
		t.Errorf("the filter mutated the action list: acts.length = %d, want 2", got.ActsLeft)
	}
}

// checkinTargetsSource 拉出「全部签到」的出队函数，连同它依赖的 accGroups 与
// checkinActionsFor。三者都不引用 CUR / esc，只有 capsOf 和 ACCTS 是外部名字，
// node 侧用桩替掉。
func checkinTargetsSource(t *testing.T) (accGroups, actionsFor, targets string) {
	t.Helper()
	src := poolStatsUISource(t)
	// poolStatsFuncBody 截到行首的 "}" 之前（不含它），node 那边需要成对的括号，
	// 所以这里把收尾的大括号补回来。
	accGroups = poolStatsFuncBody(t, src, "accGroups") + "\n}"
	actionsFor = checkinActionsForSource(t, src)
	targets = poolStatsFuncBody(t, src, "checkinTargets") + "\n}"
	return accGroups, actionsFor, targets
}

// TestCheckinTargetsRunsEachAccountOnce 用 node 跑真实的 checkinTargets。
//
// 它钉住批量签到的两条规则：孤立的 API key 账号整组跳过（不再点出必然失败），
// 有 JWT 兄弟的混合账号只出队一次、并且挑中 jwt 那条凭据；没有通道声明的动作
// （其它模块的每日签到）保持旧行为，每条已启用凭据各跑一次。
func TestCheckinTargetsRunsEachAccountOnce(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the batch scheduler behavior needs it to run the inline source")
	}
	accGroups, actionsFor, targets := checkinTargetsSource(t)
	blocks, err := json.Marshal([]string{accGroups, actionsFor, targets})
	if err != nil {
		t.Fatalf("encoding the source blocks for node: %v", err)
	}

	prog := `const fs = require("fs");
const out = process.argv[2];
try {
  const [accGroups, actionsFor, targets] = ` + string(blocks) + `;
  const ACCTS = {};
  const CAPS = {};
  const capsOf = n => CAPS[n] || {};
  const load = new Function("ACCTS", "capsOf",
    accGroups + "\n" + actionsFor + "\n" + targets + "\nreturn {checkinTargets, checkinActionsFor};");
  const api = load(ACCTS, capsOf);

  const zcaps = { actions: [{ id: "claim", channels: ["jwt"] }] };
  const dailyCaps = { actions: [{ id: "daily" }] };
  const jwtRow = { id: "jwt-1", label: "plan jwt", enabled: true, identity: "u-1", fields: { kind: "jwt" } };
  const keyRow = { id: "key-1", label: "coding-plan key", enabled: true, identity: "u-1", fields: { kind: "api-key" } };
  const loneKey = { id: "key-2", label: "lone key", enabled: true, identity: "u-2", fields: { kind: "api-key" } };
  const kimiA = { id: "kimi-a", label: "web", enabled: true, identity: "k-1", fields: {} };
  const kimiB = { id: "kimi-b", label: "cli", enabled: true, identity: "k-1", fields: {} };
  const parked = { id: "jwt-2", label: "parked", enabled: false, identity: "u-3", fields: { kind: "jwt" } };

  ACCTS.zcode = { accounts: [keyRow, jwtRow, loneKey, parked] };
  CAPS.zcode = zcaps;
  ACCTS.kimi = { accounts: [kimiA, kimiB] };
  CAPS.kimi = dailyCaps;

  const z = api.checkinTargets("zcode").map(x => x.id + ":" + x.label);
  const k = api.checkinTargets("kimi").map(x => x.id);
  fs.writeFileSync(out, JSON.stringify({ ok: true, z, k }));
} catch (e) {
  fs.writeFileSync(out, JSON.stringify({ ok: false, error: String((e && e.stack) || e) }));
}
`
	dir := t.TempDir()
	script := filepath.Join(dir, "targets.js")
	result := filepath.Join(dir, "targets.json")
	if err := os.WriteFile(script, []byte(prog), 0o600); err != nil {
		t.Fatalf("writing the node harness: %v", err)
	}
	if err := exec.Command(node, script, result).Run(); err != nil {
		t.Fatalf("node could not run the inline checkinTargets: %v", err)
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("node wrote no result: %v", err)
	}
	var got struct {
		OK    bool     `json:"ok"`
		Error string   `json:"error"`
		Z     []string `json:"z"`
		K     []string `json:"k"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding the node result: %v\n%s", err, raw)
	}
	if !got.OK {
		t.Fatalf("the inline checkinTargets threw: %s", got.Error)
	}
	// 混合账号只出队一次，且代表是 jwt 那条；孤立的 API key 与停用的账号都被跳过。
	if strings.Join(got.Z, ",") != "jwt-1:plan jwt" {
		t.Errorf("zcode targets = %v, want only the jwt row once", got.Z)
	}
	// 没有通道声明的动作照旧每条已启用凭据各跑一次，顺序保持。
	if strings.Join(got.K, ",") != "kimi-a,kimi-b" {
		t.Errorf("kimi targets = %v, want both daily rows in order", got.K)
	}
}
