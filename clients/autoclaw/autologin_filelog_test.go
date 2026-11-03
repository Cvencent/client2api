package autoclaw

import (
	"os"
	"strings"
	"sync"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// 自动登录的 transcript 必须同时落**文件日志**（issue #141 方向 4，按一类修）。
//
// 现场的失败形状：跑完之后按 `client2api.log` grep 是**零命中**，而那不是
// 「没发生」——transcript 只在内存环（`j.lines`，有上限）里，且环随 job 过期
// 一起消失。一小时后去查连记录都没了。与 #96 的「环/文件分面」同型。
//
// 这一类共五个模块：raccoon（v0.6.2 已修）、trae / autoclaw / codearts / loomy
// （v0.6.4 一起补齐）。这里钉两件事，缺一件修复都不成立：
//
//  1. 镜像必须挂在**唯一的那次环写入**上，不是挂在 logf 上——`finish()`（终态
//     失败信息）与取消路径都直接调那个写入点，排查时最想看的那几行恰恰会漏；
//  2. 两个面必须走同一段脱敏，否则环里和文件里的内容会分叉。
//
// 同类模块各自有一份同样的测试（raccoon 的守卫在
// autologin_visible_retry_test.go，其余三个在各自的 autologin 测试文件里）。
// ---------------------------------------------------------------------------

// TestAutoLoginTranscriptReachesTheFileLog 是 autoclaw 这一份；它是五个模块
// 共同契约的本地副本，判据与其余四份逐字对应。
func TestAutoLoginTranscriptReachesTheFileLog(t *testing.T) {
	src := readAutologinSource(t)

	if !strings.Contains(src, "logLine") {
		t.Fatal("autoJob 没有落盘通道：transcript 只在内存环里，按 client2api.log 排查永远零命中（issue #141）")
	}
	if !strings.Contains(src, "logLine: c.deps.Logf") {
		t.Error("job 构造时没有接上 deps.Logf：字段存在但没人接（issue #141）")
	}

	// 镜像在写入点里，而不是在 logf 里。
	writer := funcBody(t, src, "func (j *autoJob) appendLocked(")
	if !strings.Contains(writer, "j.logLine(") {
		t.Error("落盘没有挂在 appendLocked（唯一的环写入点）上：finish() 的终态信息与「已取消」" +
			"都直接调它，镜像挂在 logf 就会漏掉那几行（issue #141）")
	}
	if !strings.Contains(writer, "redactAutoLine(") {
		t.Error("落盘没有走统一脱敏：环里的副本与文件里的副本可能分叉（issue #141）")
	}
	// 反向：logf 里不该有第二处镜像（同一行会落两次）。
	logf := funcBody(t, src, "func (j *autoJob) logf(")
	if strings.Contains(logf, "j.logLine(") {
		t.Error("logf 里还有一处镜像：appendLocked 已经镜像过，同一行会落两次（issue #141）")
	}
}

// TestTheTerminalVerdictAlsoReachesTheFileLog 是**功能性**判据：把 finish 也
// 算进去，真跑一遍，断言终态那一行到了 sink。
//
// 这比静态 grep 强的地方在于：它证明的是「所有写入路径都经过镜像」，而静态
// 断言只能证明「写入点里写了镜像」。finish 绕过 logf 正是上一版的漏洞，只有
// 真跑一次才拦得住它。
func TestTheTerminalVerdictAlsoReachesTheFileLog(t *testing.T) {
	var mu sync.Mutex
	var got []string
	job := &autoJob{
		state:   core.AutoLoginRunning,
		logLine: func(format string, args ...any) { mu.Lock(); got = append(got, format); mu.Unlock() },
	}
	job.logf("已取号 %s", "13800000000")
	job.finish(core.AutoLoginFailed, "浏览器登录失败：图形验证码校验失败", "")

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("落盘只有 %d 行（%v）：finish() 的终态信息没有经过镜像，排查时最想看的那一行是唯一没落文件的（issue #141）", len(got), got)
	}
	// 终态那一行必须在——它是「跑完了没有、失败在哪」的答案。
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "%s") {
		t.Errorf("落盘的格式串不对劲：%v", got)
	}
}

// readAutologinSource 读本模块的 autologin.go。
func readAutologinSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("autologin.go")
	if err != nil {
		t.Fatalf("读不到 autologin.go：%v", err)
	}
	return string(b)
}

// funcBody 截取一个顶层函数的函数体（到行首 "}" 为止）。
func funcBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("源码里没有 %q", sig)
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}"); j >= 0 {
		return rest[:j]
	}
	t.Fatalf("%q 看着没有收尾", sig)
	return ""
}
