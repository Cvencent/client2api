package trae

import (
	"os"
	"strings"
	"sync"
	"testing"

	"client2api/internal/core"
)

// 现场的失败形状：跑完之后按 `client2api.log` grep 是**零命中**，而那不是
// 「没发生」——transcript 只在内存环（有上限）里，且环随 job 过期一起消失。
// 与 #96 的「环/文件分面」同型。这一类共五个模块（raccoon v0.6.2、其余四个
// v0.6.4），每个都要钉「镜像挂在唯一的环写入点上」与「两个面同一段脱敏」。

// TestTraeTranscriptReachesTheFileLog 是 trae 这一份；判据与同类模块逐字对应。
func TestTraeTranscriptReachesTheFileLog(t *testing.T) {
	src := readTraeAutologin(t)

	if !strings.Contains(src, "logLine") {
		t.Fatal("traeAutoJob 没有落盘通道：transcript 只在内存环里，按 client2api.log 排查永远零命中（issue #141）")
	}
	// trae 不保留整个 Deps，它把 logger 存成 c.log（`trae.go:106` 包的就是
	// deps.Logf，nil-safe）。接线判据因此认它自己的那个 sink。
	if !strings.Contains(src, "logLine: c.log") && !strings.Contains(src, "logLine: c.deps.Logf") {
		t.Error("job 构造时没有接上模块 logger：字段存在但没人接（issue #141）")
	}
	writer := traeFuncBody(t, src, "func (j *traeAutoJob) appendLocked(")
	if !strings.Contains(writer, "j.logLine(") {
		t.Error("落盘没有挂在 appendLocked（唯一的环写入点）上：finish() 的终态信息与「已取消」" +
			"都直接调它，镜像挂在 logf 就会漏掉那几行（issue #141）")
	}
	if !strings.Contains(writer, "redactAutoLine(") {
		t.Error("落盘没有走统一脱敏：环里的副本与文件里的副本可能分叉（issue #141）")
	}
	logf := traeFuncBody(t, src, "func (j *traeAutoJob) logf(")
	if strings.Contains(logf, "j.logLine(") {
		t.Error("logf 里还有一处镜像：appendLocked 已经镜像过，同一行会落两次（issue #141）")
	}
}

// TestTraeTerminalVerdictReachesTheFileLog 是**功能性**判据：把 finish 也算进去，
// 真跑一遍，断言终态那一行到了 sink。finish 绕过 logf 正是上一版的漏洞，静态
// grep 看不出来。
func TestTraeTerminalVerdictReachesTheFileLog(t *testing.T) {
	var mu sync.Mutex
	var got []string
	job := &traeAutoJob{
		state:   core.AutoLoginRunning,
		logLine: func(format string, args ...any) { mu.Lock(); got = append(got, format); mu.Unlock() },
	}
	job.logf("已取号 %s", "13800000000")
	job.finish(core.AutoLoginFailed, "浏览器登录失败：图形验证码校验失败", "")

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("落盘只有 %d 行（%v）：finish() 的终态信息没有经过镜像（issue #141）", len(got), got)
	}
}

func readTraeAutologin(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("autologin.go")
	if err != nil {
		t.Fatalf("读不到 autologin.go：%v", err)
	}
	return string(b)
}

// traeFuncBody 截取一个顶层函数的函数体（到行首 "}" 为止）。
func traeFuncBody(t *testing.T, src, sig string) string {
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
