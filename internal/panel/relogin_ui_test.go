package panel

import (
	"strings"
	"testing"
)

// relogin_ui_test.go pins the per-account 「重登」 button.
//
// 背景：cline / codearts 这类账号凭据过期时，面板原先只有两条路 —— 全局「全部
// 刷新」（refresh token 还有效才行），或者「添加账号 → 浏览器登录」。后者要先
// 自己在弹层里找到那个客户端、再手动走一遍登录，而且从账号表完全看不出「就是
// 这一行有问题」。现在账号行自己带一个「重登」，点了直接为这一行发起厂商登录。
//
// 这些检查钉的是接线本身：能力矩阵怎么分支、助手函数有没有被塞进别的函数里、
// 重登弹层有没有把标题和说明改对、跑完之后有没有对着这个账号再清一次标记。

// TestReloginButtonIsWiredToTheCapabilityMatrix: 手机号账号走接码，其余会登录的
// 模块走浏览器登录；两者都具备时同时给出，而且只有凭据看起来坏了的那一行
// 才渲染按钮。WorkBuddy 正好同时具备这三个能力。
func TestReloginButtonIsWiredToTheCapabilityMatrix(t *testing.T) {
	body := poolStatsFuncBody(t, reloginSource(t), "accRowHTML")

	// needsRelogin 必须是顶层函数。它被塞进 accRowHTML 里过一次，语法照样过、
	// 单测照样绿，只有点按钮才会 500，所以这里钉住它的作用域。
	if strings.Contains(body, "function needsRelogin") {
		t.Errorf("needsRelogin 被定义在 accRowHTML 内部，行外调用不到")
	}

	for _, want := range []string{
		// 手机号优先取操作员自己记的备注，其次才是模块的 label；账号 id 不参与，
		// 否则 trae 这种纯数字 id 会被错当成手机号而走错重登分支。
		`const phoneAcct = /^\d{6,15}$/.test(String(a.operator_note || a.label || "").trim());`,
		"if (needsRelogin(a)) {",
		"if (caps.login) {",
		`if (phoneAcct && (caps.auto_login || caps.sms)) {`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("accRowHTML 少了这一句能力判断：%s", want)
		}
	}

	// 两个入口都挂 data-do="relogin"，但用 data-mode 明确区分手动和接码。
	if n := strings.Count(body, `data-do="relogin"`); n != 2 {
		t.Errorf("accRowHTML 里 data-do=\"relogin\" 出现 %d 次，want 2（链接 + 接码）", n)
	}
	for _, want := range []string{
		`data-mode="link"`,
		`const phoneMode = caps.auto_login ? "auto" : "sms";`,
		`data-mode="' + phoneMode + '"`,
		">链接重登</button>",
		">接码重登</button>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("accRowHTML 少了这一条重登入口：%s", want)
		}
	}
	if !strings.Contains(body, `data-id="' + esc(a.id) + '"`) {
		t.Errorf("重登按钮没有带上账号 id")
	}
}

// TestNeedsReloginCoversTheThreeCredentialSignals: 判据取模块自己给的机器词。
// state、fields.expired、note 三条都要认，否则某个模块的过期账号就悄悄少了按钮。
func TestNeedsReloginCoversTheThreeCredentialSignals(t *testing.T) {
	body := poolStatsFuncBody(t, reloginSource(t), "needsRelogin")

	for _, want := range []string{
		`st === "invalid" || st === "expired" || st === "unauthorized" || st === "fault" || st === "account_fault"`,
		`f.expired === true`,
		`f.relogin === true`,
		`note.startsWith("account_fault")`,
		"login_required",
		"expired",
		"unauthorized",
		"no_credential",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("needsRelogin 不再认这条信号：%s", want)
		}
	}
}

// TestReloginDispatchesAndBailsOut: 按钮模式决定走链接登录还是接码自动登录；
// 两样都没有要报错，而不是静默打开一个空弹层。
func TestReloginDispatchesAndBailsOut(t *testing.T) {
	src := reloginSource(t)
	body := poolStatsFuncBody(t, src, "openRelogin")

	if !strings.Contains(body, "const phoneAcct = /^\\d{6,15}$/.test(phone);") {
		t.Errorf("openRelogin 不再把账号名当手机号来判定")
	}
	// 点击「链接登录」只开厂商授权页，不去碰接码平台。
	if !strings.Contains(body, `if (mode === "link" && loginOn(n)) return openBrowserRelogin(n, acc);`) {
		t.Errorf("openRelogin 不再把链接登录交给浏览器重登")
	}
	// WorkBuddy 同时有 login + auto_login；接码按钮必须选完整的自动登录，
	// 而不是面板侧那个只启动普通登录会话、自己再取一次码的半截流程。
	if !strings.Contains(body, `if (mode === "auto" && phoneAcct && autoOn(n)) return openAutoRelogin(n, acc, phone);`) {
		t.Errorf("openRelogin 不再把自动接码重登交给模块完整的 AutoLogin")
	}
	if !strings.Contains(body, `if (mode === "sms" && phoneAcct && smsOn(n) && !autoOn(n)) return openSMSRelogin(n, acc, phone);`) {
		t.Errorf("openRelogin 不再保留仅有 SMSProvider 时的面板接码回落")
	}
	if !strings.Contains(body, `toast(n + " 的账号既不能用接码重登，也没有浏览器登录，无法在面板里重新登录", "err")`) {
		t.Errorf("openRelogin 在两条路都不通时既不报错也不返回")
	}
	if !strings.Contains(src, `function loginOn(n) { return !!(capsOf(n || $("#addClient").value) || {}).login; }`) {
		t.Errorf("index.html 丢了 loginOn，openRelogin 的分支会直接抛错")
	}
}

// TestReloginButtonPassesItsModeToTheDispatcher: 两个入口用 data-mode 区分，
// 点击时必须把它传给 openRelogin；否则两行按钮最终还是会落到同一个旧分支。
func TestReloginButtonPassesItsModeToTheDispatcher(t *testing.T) {
	body := poolStatsFuncBody(t, reloginSource(t), "act")
	if !strings.Contains(body, `openRelogin(n, id, btn.dataset.mode || "")`) {
		t.Errorf("重登按钮没有把 data-mode 传给 openRelogin")
	}
}

// TestReloginDialogIsRebrandedForOneAccount: 复用「添加账号」弹层就必须把标题和
// 说明改掉，否则操作员以为自己正在新增一个账号。openAdd 每次都要把两者擦回默认，
// 免得下一次普通添加还挂着上一次的「重新登录」。
func TestReloginDialogIsRebrandedForOneAccount(t *testing.T) {
	src := reloginSource(t)

	for _, want := range []string{
		`<h3 id="addTitle">添加账号</h3>`,
		`<div id="addNotice" class="state" hidden></div>`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("index.html 少了重登需要的元素：%s", want)
		}
	}

	// 标题只能有一个。写这一版时真的落出过两个 h3（一个没有 id、一个 addTitle），
	// 浏览器把两行都画了出来，而只改第二行的文字从截图上看不出来。
	if n := strings.Count(src, "<h3>添加账号</h3>"); n != 0 {
		t.Errorf("添加账号弹层里有 %d 个没有 id 的旧标题，会和 addTitle 重复渲染", n)
	}
	if n := strings.Count(src, `<h3 id="addTitle">添加账号</h3>`); n != 1 {
		t.Errorf("addTitle 标题出现 %d 次，want 1", n)
	}

	openAdd := poolStatsFuncBody(t, src, "openAdd")
	for _, want := range []string{
		`$("#addTitle").textContent = "添加账号";`,
		`$("#addNotice").hidden = true; $("#addNotice").textContent = "";`,
	} {
		if !strings.Contains(openAdd, want) {
			t.Errorf("openAdd 不再把弹层擦回「添加账号」：%s", want)
		}
	}

	target := poolStatsFuncBody(t, src, "openReloginTarget")
	for _, want := range []string{
		`ADD.mode = "restore";`,
		`$("#addTitle").textContent = "重新登录";`,
		`$("#addNotice").hidden = false;`,
		`$("#addNotice").textContent = notice;`,
		`return ADD.restore;`,
	} {
		if !strings.Contains(target, want) {
			t.Errorf("openReloginTarget 少了这一步：%s", want)
		}
	}

	// 浏览器重登不能只改标题就完事：没有手机号可租，必须直接把授权流程起起来。
	browser := poolStatsFuncBody(t, src, "openBrowserRelogin")
	if !strings.Contains(browser, "await startLogin();") {
		t.Errorf("openBrowserRelogin 没有拉起登录流程")
	}
	if strings.Contains(browser, "smsGet(") {
		t.Errorf("openBrowserRelogin 不该碰接码")
	}
}

// TestReloginFinishRevivesWithAFallbackName: 接码重登有号码可报，浏览器重登只有
// 账号名，收尾的提示语不能因为 restore.phone 为空就变成「已重新登录并恢复：undefined」。
func TestReloginFinishRevivesWithAFallbackName(t *testing.T) {
	body := poolStatsFuncBody(t, reloginSource(t), "loginDone")

	if !strings.Contains(body, `const who = restore.label || restore.phone || restore.id;`) {
		t.Errorf("loginDone 丢了重登收尾的兜底名字")
	}
	if !strings.Contains(body, `"/accounts/" + aid(restore.id) + "/revive"`) {
		t.Errorf("loginDone 不再对重登的那个账号调 revive")
	}
}

// 客户端交接：tabbit 的登录只能在 Tabbit 浏览器里完成，模块直接把登录页送进去，
// 所以面板不能再给一个「在浏览器打开」——普通浏览器打开只会被弹回官网。面板改
// 为按模块给的路径导入凭据，然后走和其它重登一样的收尾。
func TestReloginHandsOffToTheVendorApp(t *testing.T) {
	src := reloginSource(t)
	if !strings.Contains(src, `id="btnHandoff"`) {
		t.Error("弹层里没有客户端交接的「读取凭据」按钮（id=btnHandoff）")
	}
	if !strings.Contains(src, `$("#btnHandoff").addEventListener("click"`) {
		t.Error("「读取凭据」按钮没有挂事件")
	}

	start := poolStatsFuncBody(t, src, "startLogin")
	for _, want := range []string{
		"st.local_app", "st.handoff_path",
		`$("#btnOpenUrl").hidden = !!ADD.handoff`,
		`$("#btnCopyUrl").hidden = !!ADD.handoff`,
		`$("#btnHandoff").hidden = !ADD.handoff`,
	} {
		if !strings.Contains(start, want) {
			t.Errorf("startLogin 少了 %q：交接时面板会把责任推回给操作员", want)
		}
	}

	reset := poolStatsFuncBody(t, src, "resetLogin")
	for _, want := range []string{`$("#btnHandoff").hidden = true`, `ADD.handoff = ""`} {
		if !strings.Contains(reset, want) {
			t.Errorf("resetLogin 不清上一次的交接状态（%q）：下一个账号会看到上一次的按钮", want)
		}
	}

	// 收尾复用重登那条路：导入的必须就是模块点名的那个凭据路径，收尾后仍然
	// 刷新账号池、仍然对重登的那个账号做收尾动作。
	imp := poolStatsFuncBody(t, src, "handoffImport")
	for _, want := range []string{`"/import"`, "ADD.handoff", "loginDone("} {
		if !strings.Contains(imp, want) {
			t.Errorf("handoffImport 少了 %q", want)
		}
	}
}

// 不是每个模块都有要清的惩罚标记。tabbit 没有运行期冷却，所以它没实现
// core.Reviver，面板对它调 revive 只会拿到 501，把一次成功的重登报成
// 「重登成功，但恢复失败」。收尾必须先看能力矩阵。
func TestReloginSkipsReviveWhenTheModuleHasNone(t *testing.T) {
	body := poolStatsFuncBody(t, reloginSource(t), "loginDone")
	if !strings.Contains(body, `(capsOf(n) || {}).revive`) {
		t.Error("loginDone 没看模块有没有 revive 能力：tabbit 这种没有惩罚状态的模块会被报成恢复失败")
	}
	// 轮询必须能被收尾动作停掉，否则交接会话会在成功提示之后继续把 pending
	// 消息刷回去。
	if !strings.Contains(body, "stopPoll()") {
		t.Error("loginDone 不停止轮询：成功提示会被下一次 pending 覆盖")
	}
}

// reloginSource 是这些检查共用的 index.html 正文。
func reloginSource(t *testing.T) string {
	t.Helper()
	return string(indexHTML)
}

// TestAddDialogTabFollowsCapabilities: 「浏览器登录」那个 tab 其实是「不导入、不手填」
// TestAutoAddBatchControls pins the operator-facing knobs: a desired account count
// and a maximum attempt count, plus the progress line that reports both.  The
// panel owns the loop; the module interface stays one-attempt-per-job.
func TestAutoAddBatchControls(t *testing.T) {
	src := reloginSource(t)
	for _, want := range []string{
		`id="autoWant"`,
		`id="autoTries"`,
		`期望账号数`,
		`最大尝试次数`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("auto-add batch controls are missing %s", want)
		}
	}
	// The loop must stop on either condition, and must reuse the single-job route.
	start := poolStatsFuncBody(t, src, "autoStart")
	if !strings.Contains(start, `ADD.autoBatch = { want: autoInt("#autoWant"`) ||
		!strings.Contains(start, `autoInt("#autoTries"`) {
		t.Error("autoStart does not read the batch controls")
	}
	if !strings.Contains(start, `ADD.mode !== "restore"`) {
		t.Error("re-login must stay a single attempt, not a batch")
	}
	done := poolStatsFuncBody(t, src, "autoBatchDone")
	if !strings.Contains(done, "b.added >= b.want") || !strings.Contains(done, "b.attempted >= b.tries") {
		t.Error("autoBatchDone does not stop on both the target and the attempt cap")
	}
	next := poolStatsFuncBody(t, src, "autoNextAttempt")
	if !strings.Contains(next, "const res = await autoStartRun(n)") {
		t.Error("autoNextAttempt does not start the next single-job attempt")
	}
	// A failed start is one failed attempt, not the end of the batch, and the
	// loop must not recurse into itself for every failure.
	if !strings.Contains(next, "while (!autoBatchDone())") || strings.Contains(next, "autoNextAttempt();") {
		t.Error("autoNextAttempt must loop over failed starts, not end the batch or recurse")
	}
	// A duplicate terminal poll must not double-count the same job.
	if !strings.Contains(src, "ADD.autoDone === job.id") {
		t.Error("renderAutoJob does not guard against counting the same job twice")
	}
	if !strings.Contains(src, `api(cbase(n) + "/auto-login"`) {
		t.Error("the batch lost the single-job start route")
	}
}

// 的入口，自动添加和接码都挂在它下面。只实现 AutoLoginProvider/SMSProvider 的模块
// （loomy）必须落在这个 tab 上并改名为「自动登录」，而不是被推进「手动添加」；三块
// 都没有的模块（minimaxcode）则要把这个 tab 藏掉，免得出现一个点开是空的按钮。
func TestTabbitBrowserCookieCanBeReimportedFromImportTab(t *testing.T) {
	body := poolStatsFuncBody(t, reloginSource(t), "discover")
	for _, want := range []string{
		`const refreshable = c.kind === "browser-cookie" && c.importable;`,
		`(c.imported && !refreshable ? " disabled" : "")`,
		`(refreshable || (c.importable && !c.imported) ? " checked" : "")`,
		`" · 已导入，可更新"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Tabbit 浏览器 Cookie 不能在导入页重新导入：缺少 %s", want)
		}
	}
}

func TestAddDialogTabFollowsCapabilities(t *testing.T) {
	body := poolStatsFuncBody(t, reloginSource(t), "pickAddClient")

	if !strings.Contains(body, `const canLoginTab = loginOn(n) || autoOn(n) || smsOn(n);`) {
		t.Errorf("pickAddClient 不再把自动添加/接码算进「登录」tab 的存在条件")
	}
	if !strings.Contains(body, `loginTab.hidden = !canLoginTab;`) {
		t.Errorf("pickAddClient 不再按能力隐藏「登录」tab")
	}
	if !strings.Contains(body, `loginTab.textContent = loginOn(n) ? "浏览器登录" : (autoOn(n) ? "自动登录" : "接码");`) {
		t.Errorf("pickAddClient 不再按能力给「登录」tab 改名")
	}
	if !strings.Contains(body, `const canManualTab = k.manage && (((k.fields || []).length > 0) || qcTargets(n).length > 0);`) {
		t.Errorf("pickAddClient 不再按字段/快速连接能力隐藏空的手动添加 tab")
	}
	if !strings.Contains(body, `manualTab.hidden = !canManualTab;`) {
		t.Errorf("pickAddClient 不再隐藏无内容的手动添加 tab")
	}
	if !strings.Contains(body, `if (!tabs.includes(ADD.tab)) setAddTab(tabs[0] || "login");`) {
		t.Errorf("pickAddClient 没有在能力变化后切到真正存在的 tab")
	}
}
