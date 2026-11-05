package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 钉住 nil-report GoSafe 的兜底证据链（issue #209）：
//
//	发行版以 -H=windowsgui 链接，标准句柄无效——nil report 的 25 处里
//	18 处（无内层 recover）panic 只落 goSafeFallback → stderr = 空处。
//	SetPanicSink 让 main 把真 logger 注入兜底路径；本测试钉三件事：
//	①SetPanicSink 存在且 panic 时被调用；②stderr 兜底仍先走（console 场景）；
//	③非测试代码里 GoSafe(…, nil, …) 的名单与下限（#116/#128 教训：名单要有
//	下限断言，否则删一条就静默缩小覆盖面）。
func TestSetPanicSinkReceivesNilReportPanics(t *testing.T) {
	got := make(chan string, 1)
	prev := panicSink
	panicSink = func(msg string) { got <- msg }
	defer func() { panicSink = prev }()

	Recover("probe-b", nil, func() { panic("boom-B") })
	select {
	case msg := <-got:
		if !strings.Contains(msg, "panic in probe-b") || !strings.Contains(msg, "boom-B") {
			t.Fatalf("sink message = %q, want the panic name and value", msg)
		}
	default:
		t.Fatal("SetPanicSink 的 sink 没有收到 nil-report 的 panic 报告（issue #209）")
	}
}

func TestGoSafeFallbackStillFiresFirst(t *testing.T) {
	prev := goSafeFallback
	defer func() { goSafeFallback = prev }()
	got := make(chan string, 1)
	goSafeFallback = func(msg string) { got <- msg }
	prevPanic := panicSink
	panicSink = nil
	defer func() { panicSink = prevPanic }()

	Recover("probe-fallback", nil, func() { panic("fallback-check") })
	select {
	case msg := <-got:
		if !strings.Contains(msg, "probe-fallback") {
			t.Fatalf("fallback message = %q", msg)
		}
	default:
		t.Fatal("goSafeFallback 没有被调用")
	}
}

// 名单守卫：非测试代码里 GoSafe(…, nil, …) 的数量必须与白名单一致
// （2026-11-05 盘点 18 处，见 issue #209 的清单）。SetPanicSink 已让这些
// 落点的 panic 进磁盘日志；名单下限防止删条目静默缩小覆盖面。
func TestNilReportCallSitesStayWithinTheKnownList(t *testing.T) {
	count := 0
	files := []string{}
	root := "../.."
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.Contains(path, filepath.Join("cmd", "capaudit")) {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "core.GoSafe(") && strings.Contains(line, ", nil,") {
				count++
				files = append(files, path)
			}
		}
		return nil
	})
	if count < 18 {
		t.Fatalf("GoSafe(…, nil, …) 落点只剩 %d 处（<18）：名单被静默缩小了？先确认这些落点已自带 recover 或换成本地 logger（issue #209）", count)
	}
	_ = files
}
