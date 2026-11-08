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
	// 生效探测 2s + 人工接管窗 60s：无人点击时在窗口期末尾失败。
	if d := time.Since(started); d > 70*time.Second {
		t.Errorf("吞点击路径耗了 %v：接管窗 60s 应封顶", d.Round(time.Millisecond))
	}
	if n := page.clicks.Load(); n != 1 {
		t.Errorf("吞点击路径发生了 %d 次点击：未生效不该原样重试（烧号码）", n)
	}
	// v0.7.63 起死点击不再立刻失败：进入人工接管窗（窗口内无人点击才失败）。
	if !strings.Contains(err.Error(), "人工接管") {
		t.Errorf("吞点击路径没有进入人工接管窗：%v（issue #141 操作员指正）", err)
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

// popupPage models the REAL shape the operator pointed out (issue #141): the
// button click works, the widget pops the slider challenge, and the button
// never flips because submitSmsSend only runs AFTER the challenge is solved.
type popupPage struct {
	swallowedPage
}

func (p *popupPage) EvalBool(_ context.Context, expr string) (bool, error) {
	if strings.Contains(expr, "captcha-element") {
		return true, nil // 挑战弹窗已在
	}
	return false, nil // 按钮状态：发送中/已发送都没到
}

// TestCPopupChallengeIsReportedAsOpened 钉住三分支：armed+点击生效+挑战弹出 →
// 报「点击已生效：验证挑战（滑块）已弹出」，不报「点击未生效」，不截屏误称
// 现场为死点击。
func TestCPopupChallengeIsReportedAsOpened(t *testing.T) {
	shrinkRaccoonTimings(t)
	page := &popupPage{}
	err := clickAndWaitRaccoonSend(context.Background(), page, func(string, ...any) {})
	if err == nil {
		t.Fatal("挑战未完成必须报错返回")
	}
	if !strings.Contains(err.Error(), "点击已生效") || !strings.Contains(err.Error(), "滑块") {
		t.Fatalf("挑战弹出被记成了别的话：%v（issue #141 操作员指正：点了是先弹滑块）", err)
	}
	if strings.Contains(err.Error(), "未生效") {
		t.Errorf("挑战弹出被误报为死点击：%v", err)
	}
}

// takeoverPage models the takeover window: the automated click was dropped,
// but the operator's own click inside the window flips the page into the
// sent state.
type takeoverPage struct {
	swallowedPage
	operatorClicked chan struct{}
}

func newTakeoverPage() *takeoverPage {
	return &takeoverPage{operatorClicked: make(chan struct{})}
}

func (p *takeoverPage) EvalBool(_ context.Context, expr string) (bool, error) {
	if strings.Contains(expr, "captcha-element") {
		return false, nil // 无痕验证静默拒绝：挑战弹窗不出现
	}
	select {
	case <-p.operatorClicked:
		return true, nil // 操作员点过了：按钮状态翻转，状态行有字
	default:
		return false, nil
	}
}

func (p *takeoverPage) EvalString(_ context.Context, _ string) (string, error) {
	select {
	case <-p.operatorClicked:
		return "验证码已发送，请查收短信", nil
	default:
		return "", nil
	}
}

// TestDManualTakeoverContinuesTheFlow 钉住接管窗：自动点击被吞后，流程在窗
// 口内等待操作员亲手点击；操作员点击生效即返回成功（后续 waitCode 继续等短
// 信），不再报失败、不再释放号码。
func TestDManualTakeoverContinuesTheFlow(t *testing.T) {
	shrinkRaccoonTimings(t)
	oldWin := raccoonManualTakeover
	raccoonManualTakeover = 2 * time.Second
	t.Cleanup(func() { raccoonManualTakeover = oldWin })

	page := newTakeoverPage()
	done := make(chan error, 1)
	go func() {
		done <- clickAndWaitRaccoonSend(context.Background(), page, func(string, ...any) {})
	}()
	// 自动点击 2s 生效探测失败后进入接管窗；300ms 后模拟操作员点一下。
	time.Sleep(300 * time.Millisecond)
	close(page.operatorClicked)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("人工接管后流程未继续：%v（issue #141 操作员指正）", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("接管窗内人工点击后流程没有收尾")
	}
}
