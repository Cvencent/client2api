package gateway

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// 模块级 chat_timeout 的身份必须活过整条流（issue #225）。
//
// qoder 的一条真实流：TTFB 23s、正常吐 10,691 帧、300.02s 时终态标
// unexpected_eof——那是模块自己的 defaultChatTimeout=5m 到期，厂商没有任何
// 异常。根因是两段叠加：
//
//   模块半边：qoderStream.run()（qwenwork/cline 同款）在 ctx 死亡时静默
//   return——通道关闭，网关只见 io.EOF；
//   网关半边：EOF 分支一律标 unexpected_eof，EventError 分支一律标
//   upstream_error_event——都没有「本地超时」这个身份。
//
// 修法两段一起钉：模块在 DeadlineExceeded 时把身份作为 EventError 上报
// （取消仍静默），网关用 upstreamTerminalFor 把 DeadlineExceeded（且调用方
// 自己没走）标成 module_timeout。
// ---------------------------------------------------------------------------

// A. 模块在正文吐到一半后，经 EventError 上报 DeadlineExceeded：终态必须是
// module_timeout，不许落进 upstream_* 任何一族。
func TestAModuleDeadlineEventIsLabelledModuleTimeout(t *testing.T) {
	var logs bytes.Buffer
	// 先给一帧正文（流已提交、不可切平台），再让模块报自己的超时——这正是
	// issue 里的形状：TTFB 23s 正常吐流，300s 被本地超时砍断。
	srv := newTestServerWithLog(t, &testClient{
		name:   "t",
		events: []core.Event{{Type: core.EventDelta, Delta: "hello"}},
		callEvents: [][]core.Event{{
			{Type: core.EventDelta, Delta: "partial answer"},
			{Type: core.EventError, Err: context.DeadlineExceeded},
		}},
	}, &logs)

	rec := chat(t, srv, streamBody)
	if rec.Code == 0 {
		t.Fatal("handler never ran")
	}
	got := logs.String()
	if !strings.Contains(got, "terminal=module_timeout") {
		t.Fatalf("模块自报的超时没有落进 module_timeout：\n%s（issue #225）", got)
	}
	if strings.Contains(got, "terminal=unexpected_eof") || strings.Contains(got, "terminal=upstream_") {
		t.Errorf("模块超时被记成上游故障——这正是 #225 报告的现象：\n%s", got)
	}
	// 线上信封必须点名超时，而不是"上游流提前中断"。
	if !strings.Contains(rec.Body.String(), "module_timeout") {
		t.Errorf("响应信封没有 module_timeout 标识：%s", rec.Body.String()[:min(300, rec.Body.Len())])
	}
}

// B. 反向钉子：真正的上游 EOF（没有结束帧）必须仍是 unexpected_eof。
// 没有 B，把 EOF 一律改叫 module_timeout 就成了反向过修（#125 的 C 测试同型）。
func TestBCleanEOFWithoutTerminalFrameStaysUnexpectedEOF(t *testing.T) {
	var logs bytes.Buffer
	// 一个 delta 后干净 EOF：finished 没被置位，走 unexpected_eof 分支。
	srv := newTestServerWithLog(t, &testClient{
		name:   "t",
		events: []core.Event{{Type: core.EventDelta, Delta: "hello"}},
	}, &logs)

	chat(t, srv, streamBody)
	got := logs.String()
	if !strings.Contains(got, "terminal=unexpected_eof") {
		t.Fatalf("干净 EOF 没有标 unexpected_eof：\n%s（反向过修检查，issue #225）", got)
	}
	if strings.Contains(got, "terminal=module_timeout") {
		t.Errorf("上游断流被误记成本地超时：\n%s", got)
	}
}

// C. 调用方取消经 Recv 返回时仍必须是 caller_canceled（#125 已修的语义不得
// 被本轮改动冲掉）。
func TestCCallerCancelThroughRecvStillCallerCanceled(t *testing.T) {
	var logs bytes.Buffer
	srv := newTestServerWithLog(t, &testClient{
		name:    "t",
		events:  []core.Event{{Type: core.EventDelta, Delta: "hello"}},
		recvErr: context.Canceled,
	}, &logs)

	chat(t, srv, streamBody)
	got := logs.String()
	if !strings.Contains(got, "terminal=caller_canceled") {
		t.Fatalf("调用方取消没有标 caller_canceled：\n%s（#125 回归）", got)
	}
}

// D. 静态钉：模块半边的三个 run 尾部都必须上报 DeadlineExceeded 身份；
// 网关半边 upstreamTerminalFor 必须存在且同时看 err 与调用方 context。
func TestDModuleRunTailsReportTheirDeadline(t *testing.T) {
	for _, f := range []string{
		"../../clients/qoder/sse.go",
		"../../clients/qwenwork/sse.go",
		"../../clients/cline/stream.go",
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Skipf("cannot read %s: %v", f, err)
		}
		src := string(b)
		if !strings.Contains(src, "errors.Is(s.ctx.Err(), context.DeadlineExceeded)") {
			t.Errorf("%s 的 run 尾部没有甄别 DeadlineExceeded：模块自己的 chat_timeout 会被网关记成上游断流（issue #225）", f)
		}
	}
	gsrc := gatewayFileForTest(t, "server.go")
	for _, want := range []string{
		"func upstreamTerminalFor",
		`return "module_timeout"`,
		"errors.Is(err, context.DeadlineExceeded) && r.Context().Err() == nil",
	} {
		if !strings.Contains(gsrc, want) {
			t.Errorf("server.go 缺少 %q：网关半边没接上（issue #225）", want)
		}
	}
	rsrc := gatewayFileForTest(t, "responses_stream.go")

	// 三处语义消费点：chat 流式的 EventError 标签（server.go）、chat 流式的
	// module_timeout 专用分支前面的标签兜底（同函数）、responses 流式终态
	// （responses_stream.go）。buffered 路径没有 terminal 日志，不计数。
	nSrv := strings.Count(gsrc, "upstreamTerminalFor(") - 1 // minus the definition line
	nRes := strings.Count(rsrc, "upstreamTerminalFor(")
	if nSrv < 1 || nRes < 1 {
		t.Errorf("upstreamTerminalFor 的调用点 server=%d responses=%d，期望 ≥1/≥1：流式终态的标签接线被拆了（issue #225）", nSrv, nRes)
	}
	if !strings.Contains(rsrc, "upstreamTerminalFor(") {
		t.Error("responses_stream.go 没有用 upstreamTerminalFor：responses 协议的模块超时仍会被记成 upstream_error_event（issue #225）")
	}
}
