package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 「暂不可签」不能把签到按钮锁死一整天。
//
// 背景（实测复现）：qoder 的每日 100 Credits 活动每天 10:00（UTC+8）开窗。模块
// 在活动还没下发/窗口还没开时返回 Skipped，面板把它存成 state=unavailable。
// 旧渲染把 unavailable 和 done 一样当成「今天结束了」，按钮直接 disabled ——
// 操作员上午点一次拿到「暂不可签」，当天剩下时间按钮就是灰的，奖励明明后来
// 开窗了也点不到。调度器从来没有这个毛病（checkinDoneForToday 只认 done、
// 会反复重跑），所以这是面板单侧的缺口。
//
// 这里钉三件事：
//   1. checkinStatusSettled 只把 done 当已完结；
//   2. checkinButtonHTML 只为「已完结」加 disabled；
//   3. 「全部签到」的派活（checkinTargets）只为「已完结」跳过。
//
// 第 1、2 条用 node 把函数真跑一遍（node 可选，没有就跳过运行时那半段）；
// 第 3 条是静态接线断言，因为 checkinTargets 引用了大量面板全局。
// ---------------------------------------------------------------------------

// checkinStatusSettledSource 截取 index.html 里那个纯判定函数，用于 node 实跑。
func checkinStatusSettledSource(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "function checkinStatusSettled(")
	if start < 0 {
		t.Fatal("index.html has no function checkinStatusSettled")
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatal("checkinStatusSettled looks unterminated")
	}
	return rest[:end+2]
}

func panelIndexSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(".", "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	return string(src)
}

// TestCheckinOnlyDoneSettlesTheDay pins the verdict itself by running the real
// function under node.
func TestCheckinOnlyDoneSettlesTheDay(t *testing.T) {
	src := panelIndexSource(t)
	fn := checkinStatusSettledSource(t, src)

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the static assertions below still cover the wiring")
	}
	script := fn + `
const cases = [
  {state:"done", want:true},
  {state:"unavailable", want:false},
  {state:"", want:false},
  {state:"cooling", want:false},
];
let bad = 0;
for (const c of cases) {
  const got = checkinStatusSettled(c);
  if (got !== c.want) { console.log("FAIL state=" + JSON.stringify(c.state) + " got=" + got + " want=" + c.want); bad++; }
}
console.log(bad === 0 ? "OK" : "BAD=" + bad);
`
	out, runErr := exec.Command(node, "-e", script).CombinedOutput()
	if runErr != nil {
		t.Fatalf("node run failed: %v\n%s", runErr, out)
	}
	if got := strings.TrimSpace(string(out)); got != "OK" {
		t.Fatalf("checkinStatusSettled verdicts are wrong:\n%s", out)
	}
}

// TestCheckinButtonStaysClickableWhenUnavailable is the static wiring assertion:
// the disabled attribute must come from "settled", not from "a status exists".
func TestCheckinButtonStaysClickableWhenUnavailable(t *testing.T) {
	src := panelIndexSource(t)

	start := strings.Index(src, "function checkinButtonHTML(")
	if start < 0 {
		t.Fatal("index.html has no function checkinButtonHTML")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}"); end >= 0 {
		body = body[:end+2]
	}

	// The old bug: `const dis = st ? " disabled" : ""` disabled on ANY status,
	// which turned one early "unavailable" into a dead button for the day.
	if strings.Contains(body, `const dis = st ?`) {
		t.Error("checkinButtonHTML still disables on any status; it must disable only when settled " +
			"(issue: one early \"unavailable\" left the button dead for the rest of the day)")
	}
	if !strings.Contains(body, "checkinStatusSettled(st)") {
		t.Error("checkinButtonHTML does not consult checkinStatusSettled, so \"unavailable\" and \"done\" cannot differ")
	}
	if !strings.Contains(body, `const dis = settled ? " disabled" : ""`) {
		t.Error("checkinButtonHTML's disabled attribute is not gated on the settled verdict")
	}
}

// TestCheckinBatchRetriesUnavailableRows pins the batch half: "全部签到" must not
// skip a row merely because it recorded "unavailable", or a later window would
// never be tried by the bulk action either.
func TestCheckinBatchRetriesUnavailableRows(t *testing.T) {
	src := panelIndexSource(t)

	start := strings.Index(src, "function checkinTargets(")
	if start < 0 {
		t.Fatal("index.html has no function checkinTargets")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end+2]
	}

	if !strings.Contains(body, "checkinStatusSettled(checkinStatusFor(") {
		t.Error("checkinTargets does not gate its skip on checkinStatusSettled; an \"unavailable\" row would " +
			"never be retried by 全部签到")
	}
	// The bare old form -- skip when any status exists -- must be gone.
	if strings.Contains(body, "if (checkinStatusFor(rows[0], x.id, rows)) return;") {
		t.Error("checkinTargets still skips on the mere presence of a status")
	}
}
