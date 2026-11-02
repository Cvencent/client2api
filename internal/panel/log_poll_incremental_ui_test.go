package panel

import (
	"os"
	"strings"
	"testing"
)

// 「优化机制建好了但实际没生效」一类（issue #113 + #112）。
//
// 两单都不是「没做优化」——机制都在，但一个被同一段代码的另一半覆盖掉、另一个
// 只做了半程。判据因此是「这条优化在运行时还能不能命中」，不是「那几行代码在不在」：
// #113 的旧测试 `logs_meta_probe_test.go` 三条 strings.Contains 全绿而缺陷在，
// 正是「文本匹配测不出判定对象被覆盖」的样本。

// TestLogPollDedupesOnTheSameStamp 钉住 #113 的第一半：meta 与正文必须写同一个戳。
//
// 旧实现给 meta 戳加 "meta:" 前缀、给正文戳加 "ok:" 前缀，于是下一拍算出的 meta
// 戳永远等于上一拍正文留下的 "ok:" 戳——早退分支一次都命不中。三个状态（正常 /
// 错误 / 频道）本来就不需要靠不同前缀区分：total 与 LOGCH 已经把它们分开了，
// 错误态另有 "err" 这个值。
func TestLogPollDedupesOnTheSameStamp(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderLogs")

	// 不许再出现「meta:」这个前缀写法——它就是覆盖的来源。
	if strings.Contains(body, `"meta:" +`) {
		t.Fatal(`日志轮询又给 meta 戳加了 "meta:" 前缀：正文写完会覆盖成 "ok:"，早退分支永远命不中（issue #113）`)
	}
	// meta 的判据必须用与正文相同的 "ok:" 形态。
	if !strings.Contains(body, `const stamp = "ok:" + String(metaTotal) + "|" + LOGCH;`) {
		t.Error("meta 的早退判据不是与正文同形的 \"ok:<total>|<ch>\"：两拍之间对不上（issue #113）")
	}
	if !strings.Contains(body, `const stamp = "ok:" + String(d.total || 0) + "|" + LOGCH;`) {
		t.Error("正文的戳形态变了：与 meta 的判据不再是同一个值（issue #113）")
	}
	// 错误态仍然要有自己的出口（#84 不许回退），且要作废游标。
	if !strings.Contains(body, `LOGSEEN = "err";`) {
		t.Error("错误态不再写 \"err\" 戳：失败后页面会停在上一次的成功内容上（issue #84 回归）")
	}
	if !strings.Contains(body, "LOGBODY = null;") {
		t.Error("出错后没有作废增量游标：下一拍会拿旧游标接续，跳掉断档那一段（issue #113）")
	}
}

// TestLogPollFetchesIncrementally 钉住 #113 的第二半：total 变了也只要新增行。
//
// 只修第一半不够——部署实例平均 2.2 秒新增一行、轮询 4 秒一拍，「total 未变」
// 几乎不成立，早退分支在真实实例上命中率很低。真正的省钱点是游标。
func TestLogPollFetchesIncrementally(t *testing.T) {
	src := poolStatsUISource(t)

	body := poolStatsFuncBody(t, src, "renderLogs")
	if !strings.Contains(body, `"/panel/api/logs?since=" + LOGBODY.total`) {
		t.Fatal("轮询没有带 since 游标：total 一变就整份重下 500 行（issue #113）")
	}
	// 游标只在「同一频道、且没倒退」时可用；否则必须整份重读，否则会拿一个
	// 属于另一个频道的行号去接续。
	if !strings.Contains(body, "LOGBODY.ch === LOGCH && LOGBODY.total <= metaTotal") {
		t.Error("游标没有校验频道与单调性：换频道或 total 回退后仍会按旧游标追加（issue #113）")
	}
	// partial 响应必须接在已渲染窗口后面，整份响应必须替换。
	if !strings.Contains(body, "if (d.partial) {") || !strings.Contains(body, "all = LOG_LINES.concat(fresh);") {
		t.Error("partial 响应没有接在已渲染窗口后面：增量行会覆盖掉整页日志（issue #113）")
	}
	// 追加路径也要守住保留窗口上限，否则页面会无限增长。
	if !strings.Contains(body, "if (cap > 0 && all.length > cap)") {
		t.Error("追加路径没有按 capacity 截断：长时间开着会无限增长（issue #113）")
	}
}

// TestLogCursorIsServedByThePanel 钉住服务端那一半：`since` 必须被解析并返回
// partial 标记，而不是被当成未知参数忽略。面板侧拼了参数、服务端不认，是这类
// 「机制看着有、实际没生效」最常见的形状。
func TestLogCursorIsServedByThePanel(t *testing.T) {
	// 服务端必须真的认这个参数：面板拼了、服务端忽略，是这类「机制看着有、
	// 实际没生效」最常见的形状。测试 cwd 就是包目录。
	raw, err := os.ReadFile("logs.go")
	if err != nil {
		t.Fatalf("读不到 logs.go：%v", err)
	}
	logs := string(raw)

	if !strings.Contains(logs, `r.URL.Query().Get("since")`) {
		t.Fatal("/panel/api/logs 没有读 since 参数：面板传了游标也会被忽略（issue #113）")
	}
	if !strings.Contains(logs, "EntriesSince(since)") {
		t.Error("handler 没有走环的增量查询（issue #113）")
	}
	if !strings.Contains(logs, "Partial:  true") {
		t.Error("增量响应没有标 partial：前端无法区分「追加」与「替换」，会静默丢内容（issue #113）")
	}
	// 游标滑出保留窗口时必须退回整份，而不是返回一段接不上的行。
	if !strings.Contains(logs, "Fall through to the full window") {
		t.Error("游标接不上时没有退回整份窗口：调用方会静默跳过中间那段（issue #113）")
	}
}

// TestLogPollSkipsOverlappingTicks 钉住 #113 的边界修正：轮询重叠时必须跳过，
// 不能在途再发一拍。
//
// 触发条件（单次 body fetch 拖过 4 秒）在本机几乎不会出现，但一旦出现就是**重复
// 追加**：两拍都以同一个游标发 since，各自返回后都 concat，后回来的那份把同一批行
// 再接一遍。旧的整份替换是幂等的，所以这是增量化带进来的回归——它必须由守卫堵住，
// 而不是靠「按 total 过滤」只救恰好同游标的那一种重叠。
func TestLogPollSkipsOverlappingTicks(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderLogs")

	// 守卫存在且在发请求之前生效。
	if !strings.Contains(body, "if (LOG_BUSY) return;") {
		t.Fatal("日志轮询没有 in-flight 守卫：两拍重叠时同一批行会被追加两次（issue #113）")
	}
	guard := strings.Index(body, "if (LOG_BUSY) return;")
	req := strings.Index(body, `api("/panel/api/logs?meta=1")`)
	if guard < 0 || req < 0 || guard > req {
		t.Error("守卫不在第一次请求之前：重叠的那一拍照样会发出去（issue #113）")
	}
	// 复位必须在 finally 里——成功/失败/早退三条路都要复位，卡死的守卫比重复追加更糟。
	if !strings.Contains(body, "LOG_BUSY = false;") {
		t.Fatal("守卫没有复位点：一次异常就会让日志页永远不再轮询（issue #113）")
	}
	if !strings.Contains(body, "} finally {") {
		t.Error("守卫的复位不在 finally 里：失败路径会把守卫留在 true（issue #113）")
	}
	// 复位点必须在函数体内（早退分支不会执行到），且整段被 try 包住。
	tryAt := strings.Index(body, "try {")
	firstReq := strings.Index(body, `const meta = await api(`)
	if tryAt < 0 || firstReq < 0 || tryAt > firstReq {
		t.Error("try 块没有包住第一次请求：失败时 finally 也不会跑到（issue #113）")
	}
}
