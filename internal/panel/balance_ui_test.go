package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// balance_ui_test.go pins the accounts page's balance column to the cached,
// paced contract behind it: opening the page must not ask every vendor at once.
//
// 背景：余额列原来每次打开账号页都对整个号池 fan-out 一次，账号一多就是一次
// 能触发风控的突发。现在服务端读缓存、分小批慢刷，界面这边只做两件事：读缓存
// （GET /balances），以及工具栏那个「刷新余额」按钮触发一批新的
// （POST /balances/refresh），然后分几次回读缓存把新数字补上。

func TestBalancesUIReadsTheCacheAndRefreshesInBatches(t *testing.T) {
	src := string(indexHTML)

	load := poolStatsFuncBody(t, src, "loadBalances")
	if !strings.Contains(load, "readBalances(n)") {
		t.Error("loadBalances 不再读服务端的余额缓存")
	}
	if !strings.Contains(load, "pollBalanceCache(n)") {
		t.Error("loadBalances 不再回读缓存，慢刷新后的数字不会显示")
	}
	if !strings.Contains(load, `api(cbase(n) + "/balances/refresh", { method: "POST" })`) {
		t.Error("工具栏的「刷新余额」没有走后端的分批刷新接口")
	}

	bulk := poolStatsFuncBody(t, src, "bulk")
	if !strings.Contains(bulk, "loadBalances(CUR, true)") {
		t.Error(`bulk("balance") 不再触发分批刷新`)
	}
	// 老实现是直接 loadBalances(CUR, true).then(...)，等一次全量结果回来才报数。
	if strings.Contains(bulk, "余额已刷新：") {
		t.Error(`bulk("balance") 还在等一次全量刷新返回`)
	}
}

func TestBalancesUIRendersUnknownCreditsAsDash(t *testing.T) {
	body := poolStatsFuncBody(t, string(indexHTML), "accBalanceCell")
	if !strings.Contains(body, "if (b.credits == null)") {
		t.Error("accBalanceCell 没有把「还没读到」和 0 区分开")
	}
	if !strings.Contains(body, `title="还没有读取过这个账号的余额"`) {
		t.Error("accBalanceCell 的「-」没有说明它为什么是空的")
	}
}

func TestBalancesUIMarksUnverifiedCreditsAsPending(t *testing.T) {
	body := poolStatsFuncBody(t, string(indexHTML), "accBalanceCell")
	for _, want := range []string{"b.unverified", "待确认", "厂商的两套额度数据互相矛盾"} {
		if !strings.Contains(body, want) {
			t.Errorf("accBalanceCell does not mark unverified credit: missing %q", want)
		}
	}
}

func TestBalancesUIPollsTheCacheWithoutCallingTheVendor(t *testing.T) {
	src := string(indexHTML)
	if !strings.Contains(src, "const BAL_SOFT_MS") {
		t.Error("index.html 丢了余额重新进入页面的软窗口")
	}
	ensure := poolStatsFuncBody(t, src, "ensureBalances")
	if !strings.Contains(ensure, "st.loaded && Date.now() - (st.loadedAt || 0) < BAL_SOFT_MS") {
		t.Error("重新进入账号页不再受软窗口节流，会连着要批次")
	}
	body := poolStatsFuncBody(t, src, "pollBalanceCache")
	if !strings.Contains(body, "readBalances(n)") {
		t.Error("pollBalanceCache 不再回读缓存")
	}
	// 回读走的是 GET /balances，绝不是逐账号的厂商查询。
	if strings.Contains(body, "/accounts/") {
		t.Error("轮询余额时打到了逐账号接口")
	}
	read := poolStatsFuncBody(t, src, "readBalances")
	if !strings.Contains(read, `api(cbase(n) + "/balances")`) {
		t.Error("readBalances 没有读余额缓存")
	}
	// GET 是缓存读取；这里出现写方法就说明回读变成了另一次刷新。
	if strings.Contains(read, "method: \"POST\"") {
		t.Error("readBalances 用了 POST，缓存回读不再是副作用读取")
	}
}

func TestBalancesUIAppliesTheServerDisplayScale(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the scale behavior needs it to run the inline renderer")
	}
	body := poolStatsFuncBody(t, string(indexHTML), "balDisplayNum") + "\n}\n" +
		poolStatsFuncBody(t, string(indexHTML), "accBalanceCell") + "\n}"
	blockJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	prog := `const fs = require("fs");
const out = process.argv[2];
try {
  const body = ` + string(blockJSON) + `;
  const CUR = "relay";
  const BALS = { relay: { unit: "USD", byID: { a1: { id: "a1", credits: 5000000, credits_total: 6000000, used: 2, scale: 0.000002, fetched_at: Date.now() } } } };
  const esc = s => String(s == null ? "" : s);
  const numf = n => Number(n).toLocaleString("en-US");
  const balFetchedTip = () => "";
  const fn = new Function("CUR", "BALS", "esc", "numf", "balFetchedTip", body + "\nreturn accBalanceCell;")(CUR, BALS, esc, numf, balFetchedTip);
  const html = fn({ id: "a1" });
  fs.writeFileSync(out, JSON.stringify({ ok: true, html }));
} catch (e) {
  fs.writeFileSync(out, JSON.stringify({ ok: false, error: String((e && e.message) || e) }));
}
`
	dir := t.TempDir()
	script := filepath.Join(dir, "balance-scale.js")
	result := filepath.Join(dir, "balance-scale.json")
	if err := os.WriteFile(script, []byte(prog), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(node, script, result).Run(); err != nil {
		t.Fatalf("node could not run accBalanceCell: %v", err)
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		HTML  string `json:"html"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding node result: %v\n%s", err, raw)
	}
	if !got.OK {
		t.Fatalf("accBalanceCell threw: %s", got.Error)
	}
	if !strings.Contains(got.HTML, ">10<span class=\"u\">USD</span>") {
		t.Fatalf("scaled balance = %s, want 10 USD", got.HTML)
	}
	if !strings.Contains(got.HTML, "剩余 10 / 总量 12") {
		t.Fatalf("scaled tooltip = %s, want 10 / 12 USD", got.HTML)
	}
	if strings.Contains(got.HTML, "5,000,000") || strings.Contains(got.HTML, "6,000,000") {
		t.Fatalf("raw quota units leaked into the row: %s", got.HTML)
	}
}

func TestBalancesUIClipsLongValuesInsideTheBalanceColumn(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		"#view-accounts table.acc td.c-bal { overflow: hidden; }",
		"display: block; max-width: 100%; overflow: hidden; text-overflow: ellipsis;",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("balance column has no overflow guard: missing %q", want)
		}
	}
}

func TestBalancesUICompactsLargeTokenCounts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the compact balance behavior needs it to run the inline renderer")
	}
	body := poolStatsFuncBody(t, string(indexHTML), "balDisplayNum") + "\n}\n" +
		poolStatsFuncBody(t, string(indexHTML), "accBalanceCell") + "\n}"
	blockJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	prog := `const fs = require("fs");
const out = process.argv[2];
try {
  const body = ` + string(blockJSON) + `;
  const CUR = "zcode";
  const BALS = { zcode: { unit: "tokens", byID: { a1: { id: "a1", credits: 102000000, credits_total: 108000000, fetched_at: Date.now() } } } };
  const esc = s => String(s == null ? "" : s);
  const numf = n => Number(n).toLocaleString("en-US");
  const balFetchedTip = () => "";
  const fn = new Function("CUR", "BALS", "esc", "numf", "balFetchedTip", body + "\nreturn accBalanceCell;")(CUR, BALS, esc, numf, balFetchedTip);
  const html = fn({ id: "a1" });
  fs.writeFileSync(out, JSON.stringify({ ok: true, html }));
} catch (e) {
  fs.writeFileSync(out, JSON.stringify({ ok: false, error: String((e && e.message) || e) }));
}
`
	dir := t.TempDir()
	script := filepath.Join(dir, "balance-tokens.js")
	result := filepath.Join(dir, "balance-tokens.json")
	if err := os.WriteFile(script, []byte(prog), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(node, script, result).Run(); err != nil {
		t.Fatalf("node could not run accBalanceCell: %v", err)
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		HTML  string `json:"html"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding node result: %v\n%s", err, raw)
	}
	if !got.OK {
		t.Fatalf("accBalanceCell threw: %s", got.Error)
	}
	if !strings.Contains(got.HTML, ">1.02亿<span class=\"u\">tokens</span>") {
		t.Fatalf("large token balance was not compacted: %s", got.HTML)
	}
	if !strings.Contains(got.HTML, "剩余 102,000,000 / 总量 108,000,000") {
		t.Fatalf("compact balance hid the exact amount from the tooltip: %s", got.HTML)
	}
}

func TestBalancesUIRendersAdditionalMeters(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the additional-meter renderer needs it")
	}
	body := poolStatsFuncBody(t, string(indexHTML), "balDisplayNum") + "\n}\n" +
		poolStatsFuncBody(t, string(indexHTML), "accBalanceCell") + "\n}"
	blockJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	prog := `const fs = require("fs");
const out = process.argv[2];
try {
  const body = ` + string(blockJSON) + `;
  const CUR = "doubao";
  const BALS = { doubao: { unit: "积分", byID: { a1: { id: "a1", credits: 120, credits_total: 0, fetched_at: Date.now(), lines: [{ label: "循环额度", used_percent: 15, remaining_percent: 85, resets_at: "2026-10-14T20:20:26+08:00" }] } } } };
  const esc = s => String(s == null ? "" : s);
  const numf = n => Number(n).toLocaleString("en-US");
  const balFetchedTip = () => "";
  const fn = new Function("CUR", "BALS", "esc", "numf", "balFetchedTip", body + "\nreturn accBalanceCell;")(CUR, BALS, esc, numf, balFetchedTip);
  const html = fn({ id: "a1" });
  fs.writeFileSync(out, JSON.stringify({ ok: true, html }));
} catch (e) {
  fs.writeFileSync(out, JSON.stringify({ ok: false, error: String((e && e.message) || e) }));
}
`
	dir := t.TempDir()
	script := filepath.Join(dir, "balance-lines.js")
	result := filepath.Join(dir, "balance-lines.json")
	if err := os.WriteFile(script, []byte(prog), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(node, script, result).Run(); err != nil {
		t.Fatalf("node could not run accBalanceCell: %v", err)
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		HTML  string `json:"html"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding node result: %v\n%s", err, raw)
	}
	if !got.OK {
		t.Fatalf("accBalanceCell threw: %s", got.Error)
	}
	for _, want := range []string{
		"循环额度",
		"剩余 85%",
		"已用 15%",
		"2026-10-14T20:20:26+08:00",
	} {
		if !strings.Contains(got.HTML, want) {
			t.Fatalf("additional meter rendering lost %q: %s", want, got.HTML)
		}
	}
}
