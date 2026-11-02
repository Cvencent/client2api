package workbuddy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// 板子上「没有这一行」时的取证（issue #64）。
//
// 批任务连续 8 晚 100% 被拒、零成功，而日志里只有一句
// `this account's board has no task "black_cat"`——没有任何板子原始证据，
// 于是分不清是 vendor 下线了/改名了，还是模块自己查错了通道或码。一条把
// 「这一拍板子上实际有哪些码」记下来的日志就能一次定论。

// captureLogs 收 Client 打出来的日志行。
type captureLogs struct {
	mu   sync.Mutex
	seen []string
}

func (c *captureLogs) logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, fmt.Sprintf(format, args...))
}

func (c *captureLogs) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

// TestBoardMissLogsTheCodesItActuallyCarried 钉住 #64：查不到该码时，日志必须
// 给出板子上实际有的码。没有这一行，「vendor 没了」和「我们查错了」无法区分，
// 而这两种情况的处置完全不同（一个是停批，一个是修通道/修码）。
func TestBoardMissLogsTheCodesItActuallyCarried(t *testing.T) {
	s := &taskStub{board: func(_ bool, _ int) string {
		return boardJSON(
			taskRowJSON("first_buddy", "accepted", 1, 1),
			taskRowJSON("growth_daily", "accepted", 1, 1),
		)
	}}
	c, _ := panelClient(t, &fakeRT{handler: s.handle}, cnAccountFiles())
	logs := &captureLogs{}
	c.deps.Logf = logs.logf

	// black_cat 是板子上的真实码（tasks.go 的 taskRunnerTable），但这块板子把它
	// 撤掉了——正是 #64 假设的「vendor 下线/改名」场景。走公开的 RunTask 路径，
	// 因为取证日志要能在操作员真正触发的路上出现。
	res, err := c.RunTask(context.Background(), "", "black_cat")
	if err != nil {
		t.Fatalf("RunTask 返回了错误（refused 应当是结果而不是错误）：%v", err)
	}
	if res.OK {
		t.Fatal("板子上没有这个码却报成功")
	}

	lines := logs.all()
	if len(lines) == 0 {
		t.Fatal("查不到该码时没有任何日志：无法判断 vendor 是否还有这个任务（issue #64）")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "black_cat") {
		t.Errorf("日志里没有出现被查的码：%q", joined)
	}
	// 关键的取证：板子上实际有哪些码。
	for _, want := range []string{"first_buddy", "growth_daily"} {
		if !strings.Contains(joined, want) {
			t.Errorf("日志没有列出板子实际持有的码 %q：这行才是区分「vendor 下线」与「查错地方」的证据\n%s", want, joined)
		}
	}
}

// TestBoardMissLogIsBounded 钉住取证日志不会反过来变成新的问题：板子上码多时
// 只列前若干个并标注还有多少，不把一行日志撑成几 KB。
func TestBoardMissLogIsBounded(t *testing.T) {
	rows := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		rows = append(rows, taskRowJSON(fmt.Sprintf("code_%02d", i), "accepted", 1, 1))
	}
	s := &taskStub{board: func(_ bool, _ int) string { return boardJSON(rows...) }}
	c, _ := panelClient(t, &fakeRT{handler: s.handle}, cnAccountFiles())
	logs := &captureLogs{}
	c.deps.Logf = logs.logf

	// 结果不看：本测试只关心日志的形状。用「板子上的真实码」触发 miss 路径。
	if _, err := c.RunTask(context.Background(), "", "black_cat"); err != nil {
		t.Fatalf("RunTask：%v", err)
	}
	joined := strings.Join(logs.all(), "\n")
	if !strings.Contains(joined, "more") {
		t.Errorf("码多时没有标注「还有 N 个」：一行日志可能撑到几 KB\n%s", joined)
	}
	if len(joined) > 2000 {
		t.Errorf("取证日志 %d 字节，过长：应当截断（issue #64）", len(joined))
	}
}
