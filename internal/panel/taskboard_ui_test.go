package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 任务看板「操作」列的三条分支。
//
// 背景：core.TaskInfo.Note 是模块唯一能向操作员解释"这一行是什么、点执行会
// 发生什么"的字段。面板原来只在"需人工完成"那一条分支里读它，于是所有
// Auto（可自动执行）行的 note 全被静默丢弃 —— zcode 写的"将领取优先级最高的
// 套餐（共 N 个可领取）"、loomy 写的"点执行 = 代你向厂商上报该任务已完成，
// 网关无法替你完成动作本身"都看不见，操作员只能看见一个没有任何说明的按钮。
//
// 这一组检查把三条分支逐字钉住：auto 行必须是"按钮 + note"，manual 行必须
// 在 note 为空时退回"需人工完成"，running 行保持原样。
// ---------------------------------------------------------------------------

const (
	tbRunningAction = `if (live) act = '<span class="state">执行中…</span>';`
	tbAutoAction    = `else if (t.auto) act = '<button class="xs" data-run="' + esc(t.code) + '">执行</button>' +`
	tbAutoNote      = `(t.note ? '<div class="hint">' + esc(t.note) + "</div>" : "");`
	tbManualAction  = `else act = '<span class="hint">' + esc(t.note || "需人工完成") + "</span>";`
)

// taskBoardActionWiring 返回 renderTaskBoard 里"操作"列写得不对的地方。
func taskBoardActionWiring(t *testing.T, src string) []string {
	t.Helper()
	body := poolStatsFuncBody(t, src, "renderTaskBoard")

	var bad []string
	if !strings.Contains(body, tbRunningAction) {
		bad = append(bad, "执行中的行不再显示「执行中…」")
	}
	if !strings.Contains(body, tbManualAction) {
		bad = append(bad, "需人工完成的行丢了 note 的兜底文案")
	}

	autoAt := strings.Index(body, tbAutoAction)
	if autoAt < 0 {
		bad = append(bad, "auto 行不再渲染「执行」按钮")
		return bad
	}
	noteAt := strings.Index(body, tbAutoNote)
	if noteAt < 0 {
		bad = append(bad, "auto 行丢了模块写的 note：Note 是模块唯一的解释渠道，不能只留按钮")
		return bad
	}
	if noteAt < autoAt {
		bad = append(bad, "note 出现在「执行」按钮之前，说明这段拼接被人改过")
		return bad
	}
	// 按钮与 note 必须紧挨着，中间不能插进别的分支。
	if between := body[autoAt+len(tbAutoAction) : noteAt]; strings.TrimSpace(between) != "" {
		bad = append(bad, "「执行」按钮与它的 note 之间多了别的东西："+strings.TrimSpace(between))
	}
	return bad
}

func TestTaskBoardShowsTheNoteOnAnAutomaticRowToo(t *testing.T) {
	src := poolStatsUISource(t)
	if bad := taskBoardActionWiring(t, src); len(bad) != 0 {
		for _, b := range bad {
			t.Errorf("任务看板操作列：%s", b)
		}
	}
}

// 负控一：退回"只渲染按钮"的旧写法必须被抓到。
func TestTaskBoardWiringCatchesAnAutoRowThatDropsTheNote(t *testing.T) {
	src := poolStatsUISource(t)
	old := tbAutoAction + "\n      " + tbAutoNote
	if !strings.Contains(src, old) {
		t.Fatal("测试自己失效了：找不到 auto 行的两行拼接")
	}
	mutated := strings.Replace(src, old, strings.TrimSuffix(tbAutoAction, " +")+";", 1)
	if bad := taskBoardActionWiring(t, mutated); len(bad) == 0 {
		t.Error("auto 行丢掉 note 的写法没有被抓到")
	}
}

// 负控二：manual 行把"需人工完成"的兜底删掉必须被抓到。
func TestTaskBoardWiringCatchesAManualRowWithoutAFallback(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, tbManualAction) {
		t.Fatal("测试自己失效了：找不到 manual 行")
	}
	mutated := strings.Replace(src, tbManualAction,
		`else act = '<span class="hint">' + esc(t.note) + "</span>";`, 1)
	if bad := taskBoardActionWiring(t, mutated); len(bad) == 0 {
		t.Error("manual 行丢掉兜底文案的写法没有被抓到")
	}
}

// 负控三：把 note 挪到按钮前面（看起来还"显示了 note"，但拼接语义变了）必须被抓到。
func TestTaskBoardWiringCatchesTheNoteMovedBeforeTheButton(t *testing.T) {
	src := poolStatsUISource(t)
	old := tbAutoAction + "\n      " + tbAutoNote
	if !strings.Contains(src, old) {
		t.Fatal("测试自己失效了：找不到 auto 行的两行拼接")
	}
	swapped := tbAutoNote + "\n      " + tbAutoAction
	mutated := strings.Replace(src, old, swapped, 1)
	if bad := taskBoardActionWiring(t, mutated); len(bad) == 0 {
		t.Error("note 被挪到按钮之前的写法没有被抓到")
	}
}

// ---------------------------------------------------------------------------
// 任务中心「账号优先」：三个页签共享一个选中账号，看板按账号取数、单条执行把
// 账号发给服务端。少了任何一条，就会出现「页面停在 A 账号、请求打到默认账号」
// 这种看不见的错位，所以这几处接线逐字钉住。
// ---------------------------------------------------------------------------

func TestTaskCenterBindsTheSelectedAccount(t *testing.T) {
	page := poolStatsUISource(t)
	for _, want := range []string{
		"const TASKCENTER =",
		"/tasks?account=",
		"/tasks?all=1",
		"body: { account: TASKCENTER.account }",
		`id="tbAccounts"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("任务中心缺少账号绑定：%q", want)
		}
	}
	if !strings.Contains(page, "r.account === TASKCENTER.account") {
		t.Error("运行记录没有再按账号过滤")
	}
}

func TestTaskCenterCrossLinksScheduleAndBatch(t *testing.T) {
	page := poolStatsUISource(t)
	for _, want := range []string{
		"data-scboard",
		"data-qboard",
		"function tbGotoBoard(",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("任务中心缺少跨页跳转：%q", want)
		}
	}
}
