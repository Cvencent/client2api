package raccoon

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 「点击发出」≠「点击生效」（issue #229）。
//
// 现场：el.click() 的 isTrusted=false 被阿里云无痕验证组件吞掉——CDP 说点击
// 成功、页面零反应，模块等满 raccoonSendWait 后报「等待发送结果超时」，把
// 自家的死点击说成厂商不回。两半一起钉：
//  1. #sendCode/#doLogin 走 ClickTrusted（CDP 输入域，isTrusted=true）；
//  2. 点击后 2s 内按钮/状态行必须有反应，否则报「点击未生效」并与厂商脱钩。
// ---------------------------------------------------------------------------

// swallowedPage 模拟被验证组件吞掉点击的页面：ClickTrusted 报成功，但页面
// 状态永远不动（真实页面语义：生效的点击必然翻转 disabled 或状态行）。
type swallowedPage struct {
	clicks atomic.Int32
}

func (p *swallowedPage) Exists(_ context.Context, _ string) (bool, error) { return true, nil }
func (p *swallowedPage) Click(_ context.Context, _ string) (bool, error)  { return true, nil }
func (p *swallowedPage) ClickTrusted(_ context.Context, _ string) (bool, error) {
	p.clicks.Add(1)
	return true, nil
}
func (p *swallowedPage) Fill(_ context.Context, _, _ string) (bool, error)  { return true, nil }
func (p *swallowedPage) Screenshot(_ context.Context) ([]byte, error)       { return []byte("fake-png"), nil }
func (p *swallowedPage) EvalBool(_ context.Context, _ string) (bool, error) { return false, nil }
func (p *swallowedPage) EvalString(_ context.Context, _ string) (string, error) {
	return "", nil // 状态行一动不动
}

// TestASwallowedClickIsReportedAsSuch 钉住 #229 的现象本体：静默页面在 2s
// 内没有按钮/状态反应，报「点击未生效」——不再等满发送超时、不再把锅甩给
// 厂商，也不再原样重试（烧号码）。
func TestASwallowedClickIsReportedAsSuch(t *testing.T) {
	shrinkRaccoonTimings(t)
	page := &swallowedPage{}
	started := time.Now()
	err := clickAndWaitRaccoonSend(context.Background(), page, func(string, ...any) {})
	if err == nil {
		t.Fatal("吞点击的页面没有报错")
	}
	if !strings.Contains(err.Error(), "点击未生效") {
		t.Fatalf("静默点击被记成了别的话：%v（issue #229 要求点名「点击未生效」）", err)
	}
	if strings.Contains(err.Error(), "超时") {
		t.Errorf("吞点击被记成等待超时：%v（两种失败必须分开，issue #229）", err)
	}
	// 2s 的生效窗口，不是 raccoonSendWait 的全额等待。
	if d := time.Since(started); d > 6*time.Second {
		t.Errorf("死点击耗了 %v：生效探测应在 2s 量级返回（issue #229）", d.Round(time.Millisecond))
	}
	if n := page.clicks.Load(); n != 1 {
		t.Errorf("吞点击路径发生了 %d 次点击：未生效不该原样重试（烧号码）", n)
	}
}

// TestBSendAndLoginUseTheTrustedClick 钉住通道选择：验证组件绑定的两个按钮
// 必须走 ClickTrusted，el.click() 只许留作降级。
func TestBSendAndLoginUseTheTrustedClick(t *testing.T) {
	srcBytes, err := os.ReadFile("autologin.go")
	if err != nil {
		t.Skipf("cannot read autologin.go: %v", err)
	}
	src := string(srcBytes)
	for _, want := range []string{
		"page.ClickTrusted(ctx, raccoonSelSend)",
		"page.ClickTrusted(ctx, raccoonSelLogin)",
		"waitForRaccoonClickEffect",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("autologin.go 缺少 %q（issue #229）", want)
		}
	}
	// sendCode 的点击点不得再是合成点击。
	seg := src[strings.Index(src, "func clickAndWaitRaccoonSend"):]
	seg = seg[:strings.Index(seg, "\n}")+2]
	if strings.Contains(seg, "page.Click(ctx, raccoonSelSend)") {
		t.Error("sendCode 仍在走 el.click()：isTrusted=false 会被验证组件吞掉（issue #229）")
	}
}
