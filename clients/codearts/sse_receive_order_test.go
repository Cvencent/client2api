package codearts

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// Recv 与 reader 的先后顺序（由一次 CI 失败暴露）。
//
// reader 先把它解析出的每个事件送进带缓冲的 channel，最后才记录结果并关闭
// done。两个 channel 同时就绪时 select 随机挑一个，所以「事件还在缓冲区里」
// 的时候错误完全可能先被返回——随后那些事件会在错误之后交付，调用方就看到了
// 「先失败、后内容」。CI 上就是这么红的：
//
//	--- FAIL: TestStreamDeliversATerminalErrorOnce
//	    sse_test.go:495: second Recv err = <nil>, want io.EOF
//
// （第二次 Recv 拿到的是那个本该在错误之前交付的 delta。）
//
// 下面这条循环反复跑「一段内容 + 一个致命错误」的形状，断言**错误之后不再有
// 事件**——这正是上面那条路径的判据，而且比只跑一次更容易撞上竞态。

func TestATerminalErrorIsNeverFollowedByAnEvent(t *testing.T) {
	const body = "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"error_code\":\"APIG.0602\",\"error_msg\":\"Invalid token\"}\n\n"

	for i := 0; i < 500; i++ {
		s := newStream(context.Background(), io.NopCloser(strings.NewReader(body)), time.Minute, time.Minute, nil)
		var (
			sawErr  bool
			after   []core.Event
			lastErr error
		)
		// Read to the end, then try once more: everything after the first
		// terminal error must be nothing at all (io.EOF, not an event).
		for n := 0; n < 10; n++ {
			ev, err := s.Recv()
			if err != nil {
				if !sawErr {
					sawErr = true
					lastErr = err
					if errors.Is(err, io.EOF) {
						t.Fatalf("run %d: the in-stream error was delivered as io.EOF", i)
					}
					continue
				}
				break
			}
			if sawErr {
				after = append(after, ev)
			}
		}
		if !sawErr {
			t.Fatalf("run %d: the terminal error never arrived", i)
		}
		if len(after) != 0 {
			t.Fatalf("run %d: %d event(s) delivered AFTER the terminal error (%v): the reader's buffered events lost the race with done", i, len(after), after)
		}
		if !strings.Contains(lastErr.Error(), "Invalid token") {
			t.Fatalf("run %d: err = %q, want the upstream message", i, lastErr.Error())
		}
		s.Close()
	}
}

// And the mirror: a clean end must deliver every event before the EOF, in order.
func TestACleanEndDeliversEveryEventBeforeEOF(t *testing.T) {
	const body = "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n" +
		"data: [DONE]\n\n"

	for i := 0; i < 200; i++ {
		s := newStream(context.Background(), io.NopCloser(strings.NewReader(body)), time.Minute, time.Minute, nil)
		var text strings.Builder
		for n := 0; n < 10; n++ {
			ev, err := s.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("run %d: unexpected error %v", i, err)
			}
			if ev.Type == core.EventDelta {
				text.WriteString(ev.Delta)
			}
		}
		if got := text.String(); got != "ab" {
			t.Fatalf("run %d: deltas = %q, want \"ab\" (an event was dropped or reordered)", i, got)
		}
		s.Close()
	}
}

// TestRecvDrainsBufferedEventsBeforeTheTerminalError 用一个被卡住的读端把竞态
// 变成确定的顺序：reader 已经把事件写进缓冲区，然后立刻记录错误并关闭 done。
// 如果 Recv 只看 done（不先取缓冲里的事件），这里就必然拿到错误、随后才拿到
// 那个本该在它之前的事件——CI 那次 `second Recv err = <nil>` 就是这个形状。
func TestRecvDrainsBufferedEventsBeforeTheTerminalError(t *testing.T) {
	s := &stream{
		events: make(chan core.Event, 4),
		done:   make(chan struct{}),
	}
	// Produced first, then the outcome -- the reader's own order.
	s.events <- core.Event{Type: core.EventDelta, Delta: "a"}
	s.mu.Lock()
	s.err = errStreamDone
	s.mu.Unlock()
	close(s.done)

	ev, err := s.Recv()
	if err != nil {
		t.Fatalf("first Recv after done closed = %v, want the buffered event that precedes the outcome", err)
	}
	if ev.Type != core.EventDelta || ev.Delta != "a" {
		t.Fatalf("first Recv = %+v, want the buffered delta", ev)
	}
	if _, err := s.Recv(); err == nil {
		t.Fatal("second Recv returned an event; the terminal error must follow the buffered ones")
	}
}
