package gateway

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"

	"client2api/internal/core"
)

// 流内终止标签必须区分病因：调用方断开 ≠ 上游读失败（issue #125）。
//
// 两条流循环都只在**两帧之间**检查 r.Context().Err()；取消若发生在
// stream.Recv() 阻塞中，取消身份是**通过 Recv 的 err 返回的**，于是落进
// 「凡 Recv 出错都打 upstream_read_error」的统一分支。实测日志：
//
//	stream: client loomy: loomy: reading the stream: context canceled
//	terminal=upstream_read_error  events=2743
//
// 错误文本说「调用方断开」、标签说「上游读失败」——两个相反的结论。健康统计
// 不受影响（s.fail 经 failure_normalize 按 errors.Is 分流），所以这是**纯标签
// 缺陷**，但按 terminal 统计上游失败率、事后按标签排障时会被污染。
//
// 两半必须一起成立，缺一不可：A 是报告的现象，B 防止「一律改叫 caller_canceled」
// 这种反向过修（那会让标签同样失去意义）。

// A. 取消通过 Recv 返回时，标签必须是 caller_canceled。
func TestACancelDeliveredThroughRecvIsLabelledCallerCanceled(t *testing.T) {
	var logs bytes.Buffer
	// One content frame first, so the request reaches streamSSE: the error must
	// arrive from Recv INSIDE the stream loop, which is the reported case
	// (2743 events, then a cancel).  An error before the first frame is handled
	// earlier on purpose -- nothing was rendered, so a sibling may still serve.
	srv := newTestServerWithLog(t, &testClient{
		name:    "t",
		events:  []core.Event{{Type: core.EventDelta, Delta: "hello"}},
		recvErr: context.Canceled,
	}, &logs)

	rec := chat(t, srv, streamBody)
	if rec.Code == 0 {
		t.Fatal("handler never ran")
	}
	got := logs.String()
	if !strings.Contains(got, "terminal=caller_canceled") {
		t.Fatalf("取消经 Recv 返回时被记成了别的终态：\n%s\n（期望 terminal=caller_canceled，issue #125）", got)
	}
	if strings.Contains(got, "terminal=upstream_read_error") {
		t.Errorf("取消被记成上游读失败——这正是 #125 报告的现象：\n%s", got)
	}
}

// B. 真正的上游读错误仍必须是 upstream_read_error（反向钉子，防过修）。
func TestAGenuineUpstreamReadErrorStaysUpstreamReadError(t *testing.T) {
	var logs bytes.Buffer
	srv := newTestServerWithLog(t, &testClient{
		name:    "t",
		events:  []core.Event{{Type: core.EventDelta, Delta: "hello"}},
		recvErr: errors.New("read tcp: connection reset by peer"),
	}, &logs)

	chat(t, srv, streamBody)
	got := logs.String()
	if !strings.Contains(got, "terminal=upstream_read_error") {
		t.Fatalf("真正的上游读错误没有被记成 upstream_read_error：\n%s\n（反向过修：标签同样失效）", got)
	}
	if strings.Contains(got, "terminal=caller_canceled") {
		t.Errorf("非取消的上游错误被误记成调用方断开：\n%s", got)
	}
}

// C. 取消分支必须同时看 err 与 request context，且与「两帧之间」那条既有分支
// 记同一个标签与状态——只修一半会让同一个病因在统计里裂成两类（与 #125 同型）。
func TestBothCancelPathsAgreeOnLabelAndStatus(t *testing.T) {
	src := gatewayFileForTest(t, "server.go")

	if n := strings.Count(src, `terminal = "caller_canceled"`); n != 2 {
		t.Errorf("server.go 里 caller_canceled 出现 %d 次，期望 2（两帧之间 + Recv 分支）", n)
	}
	if n := strings.Count(src, "stat.status = http.StatusRequestTimeout"); n != 2 {
		t.Errorf("两条取消路径没有都记 408：出现 %d 次", n)
	}
	for _, want := range []string{
		"errors.Is(err, context.Canceled)",
		"r.Context().Err() != nil",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("Recv 的取消分支缺少 %q：取消身份会在某条路径上丢失（issue #125）", want)
		}
	}
	// 响应流那条循环同病同修（同一个仓库里两处一样的缺陷）。
	rsrc := gatewayFileForTest(t, "responses_stream.go")
	if !strings.Contains(rsrc, "errors.Is(err, context.Canceled)") {
		t.Error("responses_stream.go 的 Recv 分支没有同样的取消分流：同族缺陷只修了一处（issue #125）")
	}
}

// gatewayFileForTest reads one gateway source file so a test can pin the shape
// of a branch that only a real stream can reach.
func gatewayFileForTest(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Skipf("cannot read %s: %v", name, err)
	}
	return string(b)
}

// newTestServerWithLog is newTestServer with the logger captured, so a test can
// assert the terminal label the loop logged.
func newTestServerWithLog(t *testing.T, c *testClient, sink *bytes.Buffer) *http.Server {
	t.Helper()
	reg := core.NewRegistry()
	reg.Add(c)
	return NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(sink, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
	})
}
