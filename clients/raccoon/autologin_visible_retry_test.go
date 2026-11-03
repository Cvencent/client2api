package raccoon

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// 自动登录的两处修正（issue #63）。
//
// 一、headless 跑不过这家厂商的风控：Aliyun 无痕验证按环境信号打分，headless=new
// mint 出来的 token 被 vendor IAM 判废，于是每次尝试白烧一个接码号码。这个功能的
// 场景就是「操作员坐在机器旁点一下」，所以改成有头浏览器。
//
// 二、验证码发送被拒即整轮 abort：一句「请重新完成滑块验证」是可恢复的（组件会重新
// 武装，下一次点击 mint 新 token），过去第一次拒绝就结束整个 run 并释放号码。

// TestAutoLoginRunsAVisibleBrowser 钉住 headless=false：这是本单的主修，且很容易在
// 「顺手统一成 headless」时被改回去。
func TestAutoLoginRunsAVisibleBrowser(t *testing.T) {
	raw, err := os.ReadFile("autologin.go")
	if err != nil {
		t.Fatalf("读不到 autologin.go：%v", err)
	}
	src := string(raw)

	if strings.Contains(src, "Headless: true") {
		t.Fatal("自动登录又用 headless 浏览器：Aliyun 无痕验证在 headless 下 mint 的 token 被 vendor 拒收，每次都白烧一个接码号码（issue #63）")
	}
	// gofmt 会把结构体字面量的冒号对齐（v0.6.7 起 launch 是多行构造，变成
	// "Headless:    false"），所以判据按字段名与值各查一次，不依赖对齐。
	if !strings.Contains(src, "Headless:") || !strings.Contains(src, "false") {
		t.Error("自动登录的浏览器不是显式有头：headless 是这条路的死因，必须显式写出来")
	}
	if i := strings.Index(src, "Headless:"); i >= 0 {
		seg := src[i : i+40]
		if !strings.Contains(seg, "false") {
			t.Errorf("Headless 字段没有取 false：%q（headless 是这条路的死因，issue #63）", strings.TrimSpace(seg))
		}
	}
	// 理由必须写在代码里，否则下一个人会为了「省事」改回去。
	if !strings.Contains(src, "issue #63") {
		t.Error("有头启动旁边没有写明原因（issue #63 的风控结论）：容易被当成疏漏改回 headless")
	}
}

// TestCodeSendIsRetriedOnce 钉住有界重试：一次原地重试，第二次拒绝原样上报。
func TestCodeSendIsRetriedOnce(t *testing.T) {
	raw, err := os.ReadFile("autologin.go")
	if err != nil {
		t.Fatalf("读不到 autologin.go：%v", err)
	}
	src := string(raw)

	if !strings.Contains(src, "const attempts = 2") {
		t.Fatal("验证码发送没有有界重试：一次「请重新完成滑块验证」就结束整轮并释放号码（issue #63）")
	}
	// 重试必须有上限，不许变成循环重试（那会持续烧号码与上游风控）。
	if strings.Contains(src, "for {") && strings.Count(src, "attempts = 2") != 1 {
		t.Error("重试没有上限：对着一直说不的 vendor 反复点击只会烧掉更多号码")
	}
	// 点击 + 等结论必须抽成独立函数，重试才能只重跑这一段而不是整段准备。
	if !strings.Contains(src, "func clickAndWaitRaccoonSend(") {
		t.Error("点击与等待没有抽成独立函数：重试要么重复不了、要么把整段准备也重跑")
	}
}

// fakeSMS 是一个「拒绝一次、第二次成功」的页面替身。
type fakeSMS struct {
	clicks atomic.Int32
	// failFirst makes the first click report the vendor's challenge refusal.
	failFirst bool
	statuses  []string
	i         atomic.Int32
}

func (f *fakeSMS) Click(ctx context.Context, sel string) (bool, error) {
	n := f.clicks.Add(1)
	switch {
	case f.failFirst && n == 1:
		// 第一次点击后页面报出 vendor 的拒绝（组件要求重做验证）。
		f.statuses = []string{"图形验证码校验失败，请重新完成滑块验证"}
		f.i.Store(0)
	case n >= 2:
		// 重试的那一次：重新武装后发出去成功了。
		f.statuses = []string{"验证码已发送，请查收短信"}
		f.i.Store(0)
	}
	return true, nil
}

func (f *fakeSMS) EvalString(ctx context.Context, expr string) (string, error) {
	if f.statuses == nil {
		return "", nil
	}
	i := int(f.i.Load())
	if i >= len(f.statuses) {
		return f.statuses[len(f.statuses)-1], nil
	}
	f.i.Add(1)
	return f.statuses[i], nil
}

func (f *fakeSMS) Fill(ctx context.Context, sel, value string) (bool, error) { return true, nil }
func (f *fakeSMS) Exists(ctx context.Context, sel string) (bool, error)      { return true, nil }
func (f *fakeSMS) EvalBool(ctx context.Context, expr string) (bool, error)   { return false, nil }

// TestASingleRefusalIsRetriedThenSucceeds 端到端：第一次被拒、第二次成功，整轮成功，
// 且点击发生了两次。
func TestASingleRefusalIsRetriedThenSucceeds(t *testing.T) {
	page := &fakeSMS{failFirst: true}
	logs := 0
	err := requestRaccoonSMS(context.Background(), page, func(string, ...any) { logs++ })
	if err != nil {
		t.Fatalf("第一次拒绝后没有重试成功：%v（这正是 issue #63 的症状）", err)
	}
	if got := page.clicks.Load(); got != 2 {
		t.Fatalf("点击按钮 %d 次，期望 2 次（一次被拒 + 一次重试）", got)
	}
	if logs == 0 {
		t.Error("重试没有留下任何日志：操作员看不到「为什么又点了一次」")
	}
}

// TestTwoRefusalsAreReported 钉住上界：连续两次被拒就如实上报，不无限重试。
func TestTwoRefusalsAreReported(t *testing.T) {
	page := &alwaysRefuse{}
	err := requestRaccoonSMS(context.Background(), page, func(string, ...any) {})
	if err == nil {
		t.Fatal("两次都拒绝却报成功")
	}
	if !strings.Contains(err.Error(), "发送验证码失败") {
		t.Fatalf("错误没有透传 vendor 的原话：%v", err)
	}
	if got := page.clicks.Load(); got != 2 {
		t.Fatalf("点击 %d 次，期望恰好 2 次（有界重试）", got)
	}
}

type alwaysRefuse struct{ clicks atomic.Int32 }

func (a *alwaysRefuse) Click(ctx context.Context, sel string) (bool, error) {
	a.clicks.Add(1)
	return true, nil
}
func (a *alwaysRefuse) EvalString(ctx context.Context, expr string) (string, error) {
	return "图形验证码校验失败，请重新完成滑块验证", nil
}
func (a *alwaysRefuse) Fill(ctx context.Context, sel, value string) (bool, error) { return true, nil }
func (a *alwaysRefuse) Exists(ctx context.Context, sel string) (bool, error)      { return true, nil }
func (a *alwaysRefuse) EvalBool(ctx context.Context, expr string) (bool, error)   { return false, nil }

var _ = errors.New
var _ = io.Discard

// 自动登录的 transcript 必须**同时**落模块日志（issue #141）。
//
// 现场：失败后按 `client2api.log` grep 是**零命中**，而那不是「没发生」——
// `raccoonAutoJob.logf` 只往内存环（j.lines，上限 80）里塞，从不走 deps.Logf；
// 环还会随 job 过期一起消失，一小时后去查连 transcript 都没了。与 #96 的
// 「环/文件分面」同型。
func TestAutoLoginTranscriptReachesTheDurableLog(t *testing.T) {
	raw, err := os.ReadFile("autologin.go")
	if err != nil {
		t.Fatalf("读不到 autologin.go：%v", err)
	}
	src := string(raw)

	if !strings.Contains(src, "logLine") {
		t.Fatal("raccoonAutoJob 没有落盘通道：transcript 只在内存环里，按 client2api.log 排查永远零命中（issue #141）")
	}
	if !strings.Contains(src, "logLine: c.deps.Logf") {
		t.Error("job 构造时没有接上 deps.Logf：通道字段存在但没人接（issue #141）")
	}
	// 落盘必须挂在**唯一的那次环写入**上，而不是挂在 logf 上（issue #141）。
	//
	// 第一版把镜像写在 logf()，可 finish()（终态失败信息）与取消路径都直接调
	// appendLocked——排查时最想看的那几行恰恰是唯一没落文件的。判据因此是
	// 「appendLocked 里有 logLine」，只找 logf 是看不出这个问题的。
	body := src[strings.Index(src, "func (j *raccoonAutoJob) appendLocked("):]
	body = body[:strings.Index(body, "\n}")]
	if !strings.Contains(body, "j.logLine(") {
		t.Error("落盘没有挂在 appendLocked（唯一的环写入点）上：finish() 的终态信息与「已取消」" +
			"都直接调它，镜像挂在 logf 就会漏掉那几行（issue #141）")
	}
	if !strings.Contains(body, "redactAutoLine(") {
		t.Error("落盘没有走统一脱敏：环里的副本与文件里的副本可能分叉（issue #141）")
	}
	// 反向：logf 里不该再有第二处镜像（同一件事只有一个说话的人）。
	logf := src[strings.Index(src, "func (j *raccoonAutoJob) logf("):]
	logf = logf[:strings.Index(logf, "\n}")]
	if strings.Contains(logf, "j.logLine(") {
		t.Error("logf 里还有一处镜像：appendLocked 已经镜像过了，同一行会落两次（issue #141）")
	}
}

// 拒绝后的重试必须先给滑块留重新武装的时间（issue #141 方向 2）。
//
// 拒绝本身就意味着环境评分下降，1.5 秒后再点一次正是最容易被二次拒绝的形状。
// 这里钉「有界」而不是「多久」：常量是 var，测试可以缩短，但不能没有。
func TestARefusedChallengeGetsTimeToRearmBeforeTheRetry(t *testing.T) {
	raw, err := os.ReadFile("autologin.go")
	if err != nil {
		t.Fatalf("读不到 autologin.go：%v", err)
	}
	src := string(raw)

	if !strings.Contains(src, "raccoonRefusalRearm") {
		t.Fatal("重试前没有重新武装窗口：拒绝后立刻再点，是报告里「两次全拒」最可能的自伤形状（issue #141）")
	}
	i := strings.Index(src, "func requestRaccoonSMS(")
	body := src[i:]
	body = body[:strings.Index(body, "\n}")]
	if !strings.Contains(body, "sleepRaccoon(ctx, raccoonRefusalRearm)") {
		t.Error("重试循环里没有等重新武装：常量声明了却没用（issue #141）")
	}
	// 仍然必须有界——不许退化成无限重试（那会持续烧号码与上游风控）。
	if !strings.Contains(body, "const attempts = 2") {
		t.Error("重试上限不见了：对着一直说不的 vendor 反复点击只会烧更多号码（issue #63/#141）")
	}
}
