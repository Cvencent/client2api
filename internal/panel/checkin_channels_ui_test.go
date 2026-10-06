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
// 「领取活动套餐」走计划账单接口，只认计划 JWT；同一个账号的编码计划 API key 是它
// 的兄弟通道（面板按 Identity 把两者收成一个账号、两行通道），但 API key 那一行
// 领不了活动套餐。以前面板把客户端级的动作铺到每一行，于是 API key 行也长出一个
// 点了必然失败的领取按钮。
//
// 修法是让模块在动作上用 CheckinAction.Channels 声明它限定在哪些通道（值就是账号
// 记录里的 fields.kind），面板在 accRowHTML 里按行过滤。这里钉两件事：
//   1. accRowHTML 真的走了这个过滤器，而不是直接铺 acts；
//   2. 过滤器本身的行为正确——用 node 把它拉出来实际跑一遍，而不是只看字符串。
//
// node 只是可选的测试依赖：没有 node 就只做第 1 条的静态断言，跳过第 2 条。
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
// accRowHTML 必须先把客户端级动作按这一行的通道收窄，再去渲染按钮。
func TestAccountRowScopesCheckinActionsToTheCredentialChannel(t *testing.T) {
	src := poolStatsUISource(t)
	row := poolStatsFuncBody(t, src, "accRowHTML")
	if !strings.Contains(row, "checkinActionsFor(a, acts)") {
		t.Fatal("accRowHTML 没有按账号的通道类型收窄签到动作：编码计划 API key 行也会长出领取按钮")
	}
	// 收窄之后就不该再直接铺客户端级的那份 acts 了。
	if strings.Contains(row, "acts.forEach") || strings.Contains(row, "acts[0]") {
		t.Fatal("accRowHTML 还在直接渲染客户端级的 acts，没有全走 checkinActionsFor")
	}
	checkinActionsForSource(t, src) // 过滤器必须在，且可截取
}

// TestCheckinActionsForNarrowsByKind 用 node 把过滤器拉出来实际跑一遍。它证明的
// 是行为而不是文字：jwt 行拿到领取动作，api-key 行拿不到，没声明通道的动作照旧
// 每一行都出现。没有 node 就跳过——面板本身不需要 node。
func TestCheckinActionsForNarrowsByKind(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the row-filter behavior needs it to run the inline function")
	}
	block := checkinActionsForSource(t, poolStatsUISource(t))
	blockJSON, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("encoding the filter for node: %v", err)
	}

	// cases：账号行的 fields.kind -> 这一行应该拿到的动作 id。
	prog := `const fs = require("fs");
const out = process.argv[2];
try {
  const block = ` + string(blockJSON) + `;
  const fn = new Function(block + "\nreturn checkinActionsFor;")();
  const acts = [
    { id: "claim", label: "领取活动套餐", channels: ["jwt"] },
    { id: "daily", label: "每日签到" },
  ];
  const rows = [
    { name: "api-key channel", fields: { kind: "api-key" }, want: ["daily"] },
    { name: "jwt channel", fields: { kind: "jwt" }, want: ["claim", "daily"] },
    { name: "kind case-insensitive", fields: { kind: "JWT" }, want: ["claim", "daily"] },
    { name: "no kind reported", fields: {}, want: ["daily"] },
    { name: "no fields at all", want: ["daily"] },
  ];
  const got = rows.map(r => ({ name: r.name, want: r.want, ids: fn({ id: "x", fields: r.fields }, acts).map(x => x.id) }));
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
	if len(got.Got) != 5 {
		t.Fatalf("node returned %d cases, want 5", len(got.Got))
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
