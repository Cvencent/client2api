package scheduler

import (
	"os"
	"strings"
	"testing"
	"time"
)

// 批量结果的「语义掩盖」一类（issue #64）。
//
// refused 的语义是「该账号当晚没得领」，确实是合法结果、不该算 failed——但它
// 让「整个批 100% 全拒、零成功」在 done 行里长得像一次干净完成：
//
//	blackcat done: accounts=21 ran=21 refused=21 skipped=0 failed=0 in 15m20s
//
// 连续 8 晚都是这一行，而 vendor 那边可能早就没有这个任务了。计数本身没错，
// 缺的是「全拒」这个事实在行里说出来。

// TestReportCountsRefusalsWithoutCallingThemFailures 钉住原有语义不许被改掉：
// 拒绝不是传输失败，不能为了让日志好看就把它计成 failed（那会让真正需要重试的
// 失败混进噪声里，方向相反但同样是口径错误）。
func TestReportCountsRefusalsWithoutCallingThemFailures(t *testing.T) {
	rep := Report{Ran: 3, Refused: 3}
	if rep.Failed != 0 {
		t.Fatal("拒绝被算成了 failed：refused 是合法结果，混进 failed 会让真失败淹没")
	}
	if got := allRefusedNote(rep); got == "" {
		t.Fatal("全拒的批没有拿到任何提示尾巴：done 行仍以 failed=0 冒充「一切正常」（issue #64）")
	}
}

// TestAllRefusedNoteOnlyFiresWhenNothingSucceeded 钉住提示的触发条件：只有
// 「每一跑都被拒、且零成功、零失败」时才加尾巴。任何一条成功、或存在真的失败，
// 都说明这一批并非「vendor 已经不要这个任务了」。设计性跳过（如 workbuddy
// 国际域的号不参加活动）不算成功迹象——issue #258：`ran=6 refused=4 skipped=2
// failed=0` 与纯全拒同样是零收益跑，原版要求 skipped==0 让这种混合形态连续
// 三天以「干净」面具漏网。
func TestAllRefusedNoteOnlyFiresWhenNothingSucceeded(t *testing.T) {
	cases := []struct {
		name string
		rep  Report
		want bool
	}{
		{"全拒（报告里的形状）", Report{Ran: 21, Refused: 21}, true},
		{"有成功", Report{Ran: 20, Refused: 19}, false},
		{"有真失败", Report{Ran: 21, Refused: 18, Failed: 3}, false},
		{"什么都没跑", Report{}, false},
		{"只跑了少数几条且全拒（仍算，但要提示）", Report{Ran: 2, Refused: 2}, true},
		{"全拒+设计性跳过（#258 activity 形状）", Report{Ran: 6, Refused: 4, Skipped: 2}, true},
	}
	for _, c := range cases {
		got := allRefusedNote(c.rep)
		if (got != "") != c.want {
			t.Errorf("%s: allRefusedNote = %q，期望有无=%v", c.name, got, c.want)
		}
	}
	// 尾巴必须把「多少条」「零成功」都写出来，否则读者还得回头数计数。
	note := allRefusedNote(Report{Ran: 21, Refused: 21})
	for _, want := range []string{"21", "REFUSED", "0 succeeded"} {
		if !strings.Contains(note, want) {
			t.Errorf("提示尾巴缺少 %q：%q（issue #64）", want, note)
		}
	}
	// 混合形态的尾巴要点名 skipped 数——读者必须能一眼分清纯全拒与「跳过+全拒」。
	mixed := allRefusedNote(Report{Ran: 6, Refused: 4, Skipped: 2})
	for _, want := range []string{"4", "0 succeeded", "2 skipped"} {
		if !strings.Contains(mixed, want) {
			t.Errorf("混合形态的提示尾巴缺少 %q：%q（issue #258）", want, mixed)
		}
	}
	// 纯全拒的尾巴不带 skipped 字样（没有跳过就不提跳过）。
	if strings.Contains(note, "skipped") {
		t.Errorf("纯全拒的尾巴不该提 skipped：%q", note)
	}
}

// TestDoneLinesCarryTheAllRefusedNote 钉住两个 done 行发射点都接上了这条尾巴。
// 只接一处的话，另一条路径（单批 vs 检查批）仍然会印出无提示的那一行。
func TestDoneLinesCarryTheAllRefusedNote(t *testing.T) {
	raw, err := os.ReadFile("scheduler.go")
	if err != nil {
		t.Fatalf("读不到 scheduler.go：%v", err)
	}
	src := string(raw)

	if n := strings.Count(src, "allRefusedNote(rep)"); n != 2 {
		t.Fatalf("done 行发射点接上 allRefusedNote 的有 %d 处，期望 2 处（issue #64）", n)
	}
	// 尾巴必须进日志格式串（%s），而不是算出来丢掉。
	if !strings.Contains(src, "failed=%d in %s%s") {
		t.Error("done 行的格式串没有为提示留位置：算出来的尾巴会被丢掉（issue #64）")
	}
}

var _ = time.Second
