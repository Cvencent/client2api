package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 接码平台状态行的界面回归测试。
//
// 背景：打开「添加账号」时，面板会 POST /sms 读一次接码平台的状态（余额、
// 可用省份、卡类型、关键词）。这一程走去的是接码平台的网络接口，慢的时候要
// 等好几秒，失败也是常事（token 过期、平台抖动）。
//
// 原来的界面没有中间态也没有重试入口：
//   · 等待期间 #smsStatus 一直是上一次的文字（第一次打开是空字符串），操作员
//     看不出「正在读」还是「读完了但没数」；
//   · 失败只在文字里写一句「平台不可用：…」，那个「平台不可用」就此钉在上面，
//     除非重新打开对话框，否则没有第二次机会——而这里正是最常需要再试一次的地方。
//
// 这一版要求状态行自己会说这三件事：读取中、读到什么、以及失败了给一个「重新读取」。
// 全是 index.html 里的字符串拼接，Go 编译器管不到，所以静态钉住。
// ---------------------------------------------------------------------------

// smsStatusWiring 返回状态行接线里缺的东西；返回空切片表示全对。
// 返回列表而不是就地断言，是为了让下面的负控用同一段代码证明它真的会红。
func smsStatusWiring(t *testing.T, src string) []string {
	t.Helper()
	var miss []string

	// 1. 标牌本身：读取中要有一个会动的点，而不是一串静止文字。
	if !strings.Contains(src, `id="smsStatus"`) {
		miss = append(miss, "sms-box 里没有状态标牌 #smsStatus")
	}
	if !strings.Contains(src, `id="smsStatusDots"`) {
		miss = append(miss, "状态标牌里没有读取中的动画元素 #smsStatusDots")
	}
	if !strings.Contains(src, `id="smsStatusDots" class="dots"`) {
		miss = append(miss, "读取中的动画没有用面板既有的 .dots（@keyframes blink），会变成一个不动的小点")
	}

	// 2. 重试入口：一个「重新读取」按钮，失败后必须能被点第二次。
	if !strings.Contains(src, `id="btnSmsStatusReload"`) {
		miss = append(miss, "状态行没有「重新读取」按钮 #btnSmsStatusReload")
	}

	// 3. renderAddSms 负责每次打开对话框时把状态行拨回「没在读」：换一个客户端、
	//    或者模块没有接码能力，都不能把上一次的读取中转圈留在那里。
	body := poolStatsFuncBody(t, src, "renderAddSms")
	if !strings.Contains(body, "syncSmsStatusUI(false)") {
		miss = append(miss, "renderAddSms 没有在打开对话框时复位状态行（syncSmsStatusUI(false)）")
	}

	// 4. syncSmsStatusUI(on) 的契约：on 为真时显示转圈、禁掉重试按钮；为假时反过来。
	//    只看它真的把 on 用到了这两个元素上，不钉具体的赋值写法。
	sync := poolStatsFuncBody(t, src, "syncSmsStatusUI")
	if !strings.Contains(sync, `$("#smsStatusDots")`) || !strings.Contains(sync, "dots.hidden = !on") {
		miss = append(miss, "syncSmsStatusUI 没有在读取时显示动画点")
	}
	if !strings.Contains(sync, `$("#btnSmsStatusReload")`) || !strings.Contains(sync, "btn.disabled = on") {
		miss = append(miss, "syncSmsStatusUI 没有在读取中禁用「重新读取」，会允许并发点出一堆请求")
	}

	// 5. 读取函数：进出中间态、失败标红并留下重试指路，成功时清掉红字。
	load := poolStatsFuncBody(t, src, "smsStatusLoad")
	if !strings.Contains(load, "syncSmsStatusUI(true)") {
		miss = append(miss, "smsStatusLoad 进入读取时没有切到「读取中」")
	}
	if !strings.Contains(load, "syncSmsStatusUI(false)") {
		miss = append(miss, "smsStatusLoad 读完后没有退出「读取中」")
	}
	if !strings.Contains(load, `st.classList.remove("err")`) {
		miss = append(miss, "smsStatusLoad 成功时没有清掉上一次的错误色")
	}
	// 读取失败和平台报了 error 都要标红；两种都至少要走一次 add("err")。
	if !strings.Contains(load, `st.classList.add("err")`) {
		miss = append(miss, "smsStatusLoad 失败时没有把状态标成错误色，操作员分不清读失败和读到 0")
	}
	if !strings.Contains(load, "重新读取") {
		miss = append(miss, "smsStatusLoad 失败提示里没有告诉操作员可以点「重新读取」再试")
	}

	// 6. 事件：按钮必须绑到 smsStatusLoad，否则它只是一个摆设。
	if !strings.Contains(src, `$("#btnSmsStatusReload").addEventListener("click", smsStatusLoad)`) {
		miss = append(miss, "#btnSmsStatusReload 没有绑到 smsStatusLoad")
	}
	return miss
}

// TestSmsStatusShowsLoadingAndRetry 钉住「打开接码框不会再一脸懵」这件事。
func TestSmsStatusShowsLoadingAndRetry(t *testing.T) {
	if miss := smsStatusWiring(t, poolStatsUISource(t)); len(miss) > 0 {
		t.Fatalf("接码平台状态行接线缺东西：\n  - %s", strings.Join(miss, "\n  - "))
	}
}

// TestSmsStatusWiringCatchesAMissingRetryButton 是负控：把「重新读取」删掉之后，
// 检查器必须报出来——否则上面那条断言可能只是在自说自话。
func TestSmsStatusWiringCatchesAMissingRetryButton(t *testing.T) {
	src := poolStatsUISource(t)
	const want = `id="btnSmsStatusReload"`
	if !strings.Contains(src, want) {
		t.Fatalf("测试自己失效了：index.html 里找不到 %q", want)
	}
	if miss := smsStatusWiring(t, strings.Replace(src, want, `id="btnSmsStatusGone"`, 1)); len(miss) == 0 {
		t.Fatal("删掉「重新读取」按钮之后，检查器还是绿的")
	}
}

// TestSmsStatusWiringCatchesAMissingDotsElement 是第二个负控：把读取中的转圈删掉，
// 剩下的状态行就只会说「正在读取」而没有任何动感，检查器必须报出来。
func TestSmsStatusWiringCatchesAMissingDotsElement(t *testing.T) {
	src := poolStatsUISource(t)
	const want = `id="smsStatusDots"`
	if !strings.Contains(src, want) {
		t.Fatalf("测试自己失效了：index.html 里找不到 %q", want)
	}
	if miss := smsStatusWiring(t, strings.Replace(src, want, `id="smsStatusDotsGone"`, 1)); len(miss) == 0 {
		t.Fatal("删掉读取中的转圈之后，检查器还是绿的")
	}
}
