package panel

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 运行日志「频道筛选」的回归测试。
//
// 网关把日志按频道存（任务 / 对话 / 系统），面板按频道筛。两边都是字符串拼的：
// Go 编译不管 index.html 里的字段名，shell_test.go 那只保证 $() 指的 id 存在。
// 所以这里静态钉住：
//   1. 界面读的键 == gateway.LogEntry 的 json tag（错一个字母 = 每行都是空的
//      时间戳、空的频道标签，而且两边都不报错）；
//   2. 接口返回的是 entries 而不是旧的 lines：旧键没了，界面不能还在读它；
//   3. 四个 chip 与点击处理器都在，筛选状态真的被 renderLogs 用上；
//   4. 行类与级别类与既有 CSS 对得上（.ln / .ln.e / .ln.w / .lch.c-*）。
// ---------------------------------------------------------------------------

// logEntryJSONKeys 从 gateway.LogEntry 的字段上抽出 json tag，返回 name→tag。
func logEntryJSONKeys(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("../gateway/logring.go")
	if err != nil {
		t.Skipf("cannot read ../gateway/logring.go: %v", err)
	}
	src := string(b)

	start := strings.Index(src, "type LogEntry struct {")
	if start < 0 {
		t.Fatal("gateway.LogEntry is gone; the panel reads its fields")
	}
	end := strings.Index(src[start:], "\n}")
	if end < 0 {
		t.Fatal("LogEntry looks unterminated")
	}
	body := src[start : start+end]

	tagRe := regexp.MustCompile("`json:\"([a-z_]+)\"`")
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		m := tagRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out[fields[0]] = m[1]
	}
	return out
}

// TestLogEntryJSONKeysReachTheShell is the cross-package consistency check: the
// names the shell reads must be exactly the json tags the gateway writes.
func TestLogEntryJSONKeysReachTheShell(t *testing.T) {
	keys := logEntryJSONKeys(t)
	for _, want := range []string{"TS", "Ch", "Text"} {
		if keys[want] == "" {
			t.Fatalf("LogEntry.%s has no json tag; the panel cannot read it", want)
		}
	}
	if keys["TS"] != "ts" || keys["Ch"] != "ch" || keys["Text"] != "text" {
		t.Fatalf("LogEntry json tags changed: %v (update the shell and this test together)", keys)
	}

	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderLogs")
	for _, key := range []string{keys["TS"], keys["Ch"], keys["Text"]} {
		if !strings.Contains(body, "e."+key) {
			t.Errorf("renderLogs does not read e.%s; that field would render empty", key)
		}
	}
}

// TestLogsResponseCarriesEntriesNotLines pins the wire change: the old "lines"
// key is gone, so a shell still reading d.lines would show an empty log box
// forever with no error anywhere.
func TestLogsResponseCarriesEntriesNotLines(t *testing.T) {
	b, err := os.ReadFile("logs.go")
	if err != nil {
		t.Skipf("cannot read logs.go: %v", err)
	}
	goSrc := string(b)
	if !strings.Contains(goSrc, "`json:\"entries\"`") {
		t.Error(`logsResponse must expose Entries as json:"entries"`)
	}
	if strings.Contains(goSrc, "`json:\"lines\"`") {
		t.Error(`the "lines" key was replaced by "entries"; it must not come back`)
	}
	if !strings.Contains(goSrc, "ring.Entries(limit)") {
		t.Error("handleLogs must serve ring.Entries, which carry the channel")
	}

	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderLogs")
	if !strings.Contains(body, "d.entries") {
		t.Error("renderLogs must read d.entries")
	}
	if strings.Contains(src, "d.lines") || strings.Contains(src, "r.data.lines") {
		t.Error("the shell still reads the removed lines key somewhere")
	}
}

// TestLogChipsAreWired checks the filter controls exist in the markup and that
// the click handler really re-renders with the selected channel.
func TestLogChipsAreWired(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `id="logChips"`) {
		t.Fatal(`the logs header has no <span class="chips" id="logChips">`)
	}
	for _, ch := range []string{"all", "task", "chat", "sys"} {
		if !strings.Contains(src, `data-ch="`+ch+`"`) {
			t.Errorf("no chip for channel %q", ch)
		}
	}
	// 第一个 chip 必须默认选中，否则首屏的筛选状态与高亮不一致。
	if !strings.Contains(src, `class="xs chip on" data-ch="all"`) {
		t.Error(`the "all" chip must start selected`)
	}
	// 只读默认值：界面给的频道名必须与 gateway 的常量逐字相同。
	for _, ch := range []string{"task", "chat", "sys"} {
		if !strings.Contains(src, `"`+ch+`"`) {
			t.Errorf("channel %q is not named anywhere in the shell", ch)
		}
	}

	if !strings.Contains(src, `$("#logChips").addEventListener("click"`) {
		t.Error("the chips have no click handler")
	}
	if !strings.Contains(src, `querySelectorAll("#logChips .chip")`) {
		t.Error("the handler must move the .on highlight, not just the filter")
	}
	if !strings.Contains(src, "ev.target.closest") {
		t.Error("the handler must resolve the clicked chip through closest(), so the label text still works")
	}

	// 筛选状态必须真的进 renderLogs：只切换高亮而不过滤就是死控件。
	body := poolStatsFuncBody(t, src, "renderLogs")
	if !strings.Contains(body, "LOGCH") {
		t.Error("renderLogs ignores LOGCH, so the chips filter nothing")
	}
	if !strings.Contains(body, "all.filter(") && !strings.Contains(body, ".filter(") {
		t.Error("renderLogs does not filter by channel")
	}
}

// TestLogRowClassesMatchTheStylesheet keeps the emitted classes and the CSS in
// step.  These rules were ported with the rest of the reference stylesheet and
// were unused until the channel view started emitting them.
func TestLogRowClassesMatchTheStylesheet(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderLogs")

	for _, want := range []string{
		`class="ln`, // row class, styled by "#logBox .ln"
		`"lch c-`,   // channel tag, styled by ".ln .lch.c-*"
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderLogs no longer emits %s", want)
		}
	}
	// 频道标签只在「全部」视图里加：筛过之后每行同频道，标签就是噪声。
	if !strings.Contains(body, `LOGCH === "all"`) {
		t.Error("the channel tag must be conditional on the unfiltered view")
	}

	for _, rule := range []string{
		"#logBox .ln { display: block; }",
		"#logBox .ln.e { color: var(--bad); }",
		"#logBox .ln.w { color: var(--warn); }",
		".ln .lch.c-task",
		".ln .lch.c-chat",
		".ln .lch.c-sys",
	} {
		if !strings.Contains(src, rule) {
			t.Errorf("stylesheet is missing %q", rule)
		}
	}
	if !strings.Contains(body, "logLevelClass(") {
		t.Error("renderLogs never asks for the level class, so errors are not coloured")
	}
	// 级别类在 logLevelClass 里拼，不在 renderLogs 里。
	level := poolStatsFuncBody(t, src, "logLevelClass")
	for _, cls := range []string{`" e"`, `" w"`} {
		if !strings.Contains(level, cls) {
			t.Errorf("logLevelClass never returns the level class %s", cls)
		}
	}
	if !strings.Contains(level, "error|失败|错误") || !strings.Contains(level, "warn|冷却|熔断") {
		t.Error("the level patterns lost their Chinese words; failures in this codebase are logged in Chinese")
	}
}

// TestLogChannelLabelsAreChinese pins the operator-facing labels; they are the
// only place a raw channel id turns into something readable.
func TestLogChannelLabelsAreChinese(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{"任务", "对话", "系统"} {
		if !strings.Contains(src, want) {
			t.Errorf("channel label %q is missing from the shell", want)
		}
	}
	if !strings.Contains(src, "LOG_CH_LABEL") {
		t.Error("the channel label table is gone; rows would show raw ids like \"sys\"")
	}
}
