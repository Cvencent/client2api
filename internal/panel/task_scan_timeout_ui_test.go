package panel

import (
	"os"
	"strings"
	"testing"
	"time"
)

// 扫描/批量的「结果语义掩盖真实状况」一类（issue #59 + #64 的面板侧）。
//
// #59 的两条都不是功能缺失，而是**调度口径**：整体 2 分钟截止 + 4 并发，让
// 52 账号 × 上游 10s 的池子在慢上游日必然把队尾（正是新加的账号）掐掉；而且
// 掐掉时只留一句裸 `context deadline exceeded`，看不出是哪一个账号慢、也不知道
// 是不是整轮都废了。

// TestTaskScanHasPerAccountHeadroom 钉住 #59 的两处配置：并发要够、每账号要有子超时。
func TestTaskScanHasPerAccountHeadroom(t *testing.T) {
	raw, err := os.ReadFile("taskqueue.go")
	if err != nil {
		t.Fatalf("读不到 taskqueue.go：%v", err)
	}
	src := string(raw)

	if strings.Contains(src, "taskScanConc = 4") {
		t.Fatal("扫描并发退回 4：52 账号 × 上游 10s ÷ 4 = 130s 超过 2 分钟预算，队尾（新账号）必被掐（issue #59）")
	}
	if !strings.Contains(src, "taskScanConc = 8") {
		t.Error("扫描并发不是 8：慢上游日的池子算不过来（issue #59）")
	}
	// 并发不许无限涨——一次问整个池子会被上游当成爬虫（常量注释里写明了这条）。
	if !strings.Contains(src, "taskAccountTimeout =") {
		t.Fatal("没有每账号子超时：一个慢账号会占着并发槽直到整轮截止，把排队中的账号一起拖死（issue #59）")
	}
	// 子超时必须明显小于整轮预算，否则等于没有。
	if !strings.Contains(src, "taskScanTimeout = 2 * time.Minute") {
		t.Error("整轮预算不再是 2 分钟：子超时与预算的相对关系失去依据（issue #59）")
	}
	if !strings.Contains(src, "accountTimeoutNote") {
		t.Error("没有给「这个账号读取超时」的专用措辞：裸 context deadline exceeded 分不清是哪一个账号、也看不出其余行是否还好（issue #59）")
	}
}

// TestTaskScanPerAccountTimeoutIsWired 钉住子超时真的进了扫描循环，而不只是
// 定义了一个没人用的常量（这类「声明了没接上」正是本仓反复出现的形状）。
func TestTaskScanPerAccountTimeoutIsWired(t *testing.T) {
	raw, err := os.ReadFile("taskboard.go")
	if err != nil {
		t.Fatalf("读不到 taskboard.go：%v", err)
	}
	src := string(raw)

	if !strings.Contains(src, "context.WithTimeout(ctx, taskAccountTimeout)") {
		t.Fatal("扫描循环没有为每个账号派生带超时的子 ctx：常量定义了没人用（issue #59）")
	}
	// 子 ctx 必须在读出结果后立即释放，否则每条 goroutine 都会留下一个
	// 定时器直到它自己到点。
	idx := strings.Index(src, "context.WithTimeout(ctx, taskAccountTimeout)")
	tail := src[idx:]
	if !strings.Contains(tail[:min(len(tail), 400)], "acancel()") {
		t.Error("子 ctx 读完后没有 cancel：每个账号都留一个到期的定时器（issue #59）")
	}
	// 子超时触发要给专用措辞，且不能把整轮被掐也报成「这个账号慢」。
	if !strings.Contains(src, "errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil") {
		t.Error("超时措辞没有区分「这个账号慢」与「整轮被掐」：ctx.Err() 检查缺失（issue #59）")
	}
	if !strings.Contains(src, "accountTimeoutNote") {
		t.Error("子超时那条分支没有用专用措辞（issue #59）")
	}
	// 整轮预算常量本身不许被顺手调大来「解决」这个问题：真正的修法是子超时。
	if strings.Contains(src, "taskScanTimeout = 5 * time.Minute") {
		t.Error("用调大整轮预算来掩盖队尾超时：慢账号仍会占满并发槽（issue #59）")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = time.Second
