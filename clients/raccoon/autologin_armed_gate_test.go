package raccoon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 「组件武装」的轮询等待与死点击现场留证（issue #229 重开部分）。
//
// ClickTrusted（isTrusted=true）落地后仍被吞——吞它的不是事件可信度，而是
// 验证组件当时还没武装：raccoonSendSettle=1.5s 的固定等待是猜的，脚本冷缓存
// 期 blows past it。两半一起钉：
//  1. 页面在 initAliyunCaptcha 挂好 handler 后置 window.__captchaArmed；
//     模块点击前轮询该信号（不再猜时长）；
//  2. 「点击未生效」路径截屏落盘——下一次再有形状变化时操作员有画面可看。
// ---------------------------------------------------------------------------

// TestAArmedGateBlocksTheClickUntilThePageSignals 钉住就绪轮询：页面未武装时
// 不点击（一次都不点），武装后照常进入点击流程。
func TestAArmedGateBlocksTheClickUntilThePageSignals(t *testing.T) {
	shrinkRaccoonTimings(t)
	page := newFakeRaccoonPage()
	// 未武装：armed 信号 false，但点击一旦发生页面照常响应。
	page.exists[raccoonSelSmsTab] = true
	done := make(chan error, 1)
	go func() {
		done <- requestRaccoonSMS(context.Background(), page, func(string, ...any) {})
	}()
	time.Sleep(80 * time.Millisecond)
	if n := len(page.clicks); n != 0 {
		t.Fatalf("未武装就发生了 %d 次点击：armed 门没拦住（issue #229 重开）", n)
	}
	// 现在让 fake 的 armed 信号翻真：真实页面在 initAliyunCaptcha 返回后置
	// window.__captchaArmed，fake 用同名字段模拟。
	page.mu.Lock()
	page.armed = true
	page.mu.Unlock()
	// 武装后走一次完整流程：页面真实行为是「发送中 → 验证码已发送」，fake
	// 在 ClickTrusted 落地时直接置成功文案（send 的流程收尾就是读到它）。
	page.mu.Lock()
	page.status = "验证码已发送，请查收短信"
	page.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("武装后流程未走通：%v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("armed 翻转后流程没有收尾")
	}
}

// TestBScreenshotLandsOnSwallowedClick 钉住留证半边：吞点击路径必须把现场
// PNG 落盘（文件 + transcript 字节数两处），否则下一次形状变化还是黑箱。
func TestBScreenshotLandsOnSwallowedClick(t *testing.T) {
	shrinkRaccoonTimings(t)
	tmp := t.TempDir()
	oldDir := screenshotDir
	screenshotDir = filepath.Join(tmp, "shots")
	t.Cleanup(func() { screenshotDir = oldDir })

	page := &swallowedPage{} // 点击报成功、状态永远静默
	err := clickAndWaitRaccoonSend(context.Background(), page, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "点击未生效") {
		t.Fatalf("吞点击路径没有按 #229 报「点击未生效」：%v", err)
	}
	entries, readErr := os.ReadDir(screenshotDir)
	if readErr != nil {
		t.Fatalf("截屏目录不存在：%v（issue #229 重开：现场必须留证）", readErr)
	}
	if len(entries) != 1 {
		t.Fatalf("截屏落盘 %d 个文件，期望恰好 1", len(entries))
	}
	if !strings.HasSuffix(entries[0].Name(), ".png") {
		t.Errorf("落盘的不是 png：%s", entries[0].Name())
	}
}

// TestCLoginPageSetsTheArmedSignal 钉住页面半边：initAliyunCaptcha 挂好
// handler 后必须置 window.__captchaArmed（模块侧轮询的信号源）。
func TestCLoginPageSetsTheArmedSignal(t *testing.T) {
	src, err := os.ReadFile("login_page.go")
	if err != nil {
		t.Skipf("cannot read login_page.go: %v", err)
	}
	s := string(src)
	if !strings.Contains(s, "window.__captchaArmed = true") {
		t.Error("login page 没有置 __captchaArmed：模块侧的轮询没有信号源（issue #229 重开）")
	}
	// 信号必须挂在 initAliyunCaptcha(...) 返回之后（武装的语义）。
	i := strings.Index(s, "window.__captchaArmed = true")
	j := strings.LastIndex(s[:i], "window.initAliyunCaptcha({")
	if j < 0 {
		t.Fatal("找不到 initAliyunCaptcha 调用：锚点失效")
	}
	// 两者之间必须是同一函数体（中间没有 early return 把它截走）。
	between := s[j:i]
	if strings.Contains(between, "return;") {
		t.Error("__captchaArmed 与 initAliyunCaptcha 之间有 return：信号可能永远不置位")
	}
}
