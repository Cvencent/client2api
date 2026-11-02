package panel

import (
	"strings"
	"testing"
)

// 面板「判据/时机写了一半」一类（issue #55 + #61）。
//
// 两单同型，且都不是「某块功能坏了」，而是同一条链的两个时刻各写了一半：
//   - #55：`settle` 这个「这一页演过」的判据还在写，读它的那份代码没了，于是
//     「进场动画只演一次」变成每次切回来重演；
//   - #61：切换客户端的动作在发请求前只换对了一半（账号条换了、头部与正文没换），
//     于是慢上游的整个往返期间页面上同时摆着两个客户端的内容。
//
// 判据：一个状态/标记必须有读者（写了没人读 = 失效）；一次切换必须在等待窗口
// 开始前就把整页推进过渡态（只推一半 = 自相矛盾画面）。

// TestEnteredViewsSkipTheEntryCascade 钉住 #55：settle 必须是 fxCascade 的真判据。
//
// 这条机制曾经断成两半——go() 照旧打 settle，而读它的 JS 已经不在（历史实现是
// 一族 CSS 读 `.settle`，随「区块级编排改由 .fx-in 负责」一起被删）。剩下的一句
// 无差别 `delete el.dataset.fxDone` 又把整页重置成「没演过」，于是每次切回来
// 整页重演（配置页尾部延迟 408ms）。
func TestEnteredViewsSkipTheEntryCascade(t *testing.T) {
	src := poolStatsUISource(t)

	// 判据必须真的被读：fxCascade 首行守卫。
	cascade := poolStatsFuncBody(t, src, "fxCascade")
	if !strings.Contains(cascade, `if (host.classList.contains("settle")) return;`) {
		t.Fatal("fxCascade 没有读 .settle：判据写了没人读，切回来的视图会整页重演（issue #55）")
	}
	// go() 仍要写这个标记（写读两侧都在，机制才闭合）。
	goBody := poolStatsFuncBody(t, src, "go")
	if !strings.Contains(goBody, `shown.classList.add("settle")`) {
		t.Error("go() 不再给已进入过的视图打 .settle：判据的写入侧没了（issue #55）")
	}
	if !strings.Contains(goBody, "VIEW_ENTERED.has(v)") {
		t.Error("go() 不再用 VIEW_ENTERED 区分首演与重访（issue #55）")
	}
	// 无差别的 data-fx-done 清除不许回来：它是让静态区块跟着重演的那一句。
	if strings.Contains(goBody, `querySelectorAll("[data-fx-done]").forEach`) {
		t.Error("go() 又无差别清掉整页 data-fx-done：没被重画的静态区块会跟着重新级联（issue #55 回归）")
	}
	// 注释里的判据说明也必须与实现同侧，否则下一个人照着注释改回旧写法。
	if !strings.Contains(src, "每屏只演一次；切回来的视图带 .settle，直接就是最终状态") {
		t.Error("CSS 那段注释不再说明「切回来的视图直接是最终状态」（issue #55 的误导锚点）")
	}
}

// TestTaskBoardEntersThePendingStateBeforeTheRequest 钉住 #61：切换客户端时，
// 头部与正文必须在 await 之前就进入过渡态。
func TestTaskBoardEntersThePendingStateBeforeTheRequest(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderTaskBoard")

	idxNote := strings.Index(body, `$("#tbNote").textContent = platformLabel(TB.client) + " · 扫描中…"`)
	idxBody := strings.Index(body, `$("#tbBody").innerHTML = '<div class="pad hint">正在读取 '`)
	idxReq := strings.Index(body, "const r = await api(cbase(TB.client) + path)")
	if idxReq < 0 {
		t.Fatal("renderTaskBoard 里找不到那次 /tasks 请求（issue #61）")
	}
	if idxNote < 0 {
		t.Error("发请求前没有把 #tbNote 置为「扫描中…」：头部在整个往返期间还是上一个客户端（issue #61）")
	}
	if idxBody < 0 {
		t.Error("发请求前没有把 #tbBody 推入提示态：旧客户端的正文会和新账号条并排展示（issue #61）")
	}
	// 顺序就是这个缺陷的全部：两个过渡态必须写在 await 之前。
	if idxNote >= 0 && idxNote > idxReq {
		t.Error("#tbNote 的过渡态写在请求之后：等于没有过渡态（issue #61）")
	}
	if idxBody >= 0 && idxBody > idxReq {
		t.Error("#tbBody 的过渡态写在请求之后：矛盾画面照旧（issue #61）")
	}
	// 账号条那一句本来就在前面，钉住它别被挪到后面去（它是「矛盾画面」的起因）。
	if idxAcc := strings.Index(body, `$("#tbAccounts").innerHTML = tbAccountChips(accts)`); idxAcc < 0 || idxAcc > idxReq {
		t.Error("账号条不再是发请求前更新的：过渡态与它的先后关系变了（issue #61）")
	}
}
