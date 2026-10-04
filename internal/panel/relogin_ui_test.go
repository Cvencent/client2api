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
// 模块走浏览器登录，两者互斥；而且只有凭据看起来坏了的那一行才渲染按钮。
func TestReloginButtonIsWiredToTheCapabilityMatrix(t *testing.T) {
	body := poolStatsFuncBody(t, reloginSource(t), "accRowHTML")

	// needsRelogin 必须是顶层函数。它被塞进 accRowHTML 里过一次，语法照样过、
	// 单测照样绿，只有点按钮才会 500，所以这里钉住它的作用域。
	if strings.Contains(body, "function needsRelogin") {
		t.Errorf("needsRelogin 被定义在 accRowHTML 内部，行外调用不到")
	}

	for _, want := range []string{
		`const phoneAcct = /^\d{6,15}$/.test(String(a.label || "").trim());`,
		"if (caps.sms && phoneAcct) {",
		"} else if (caps.login && needsRelogin(a)) {",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("accRowHTML 少了这一句能力判断：%s", want)
		}
	}

	// 两条分支都要真的渲染按钮，且都挂 data-do="relogin"。
	if n := strings.Count(body, `data-do="relogin"`); n != 2 {
		t.Errorf("accRowHTML 里 data-do=\"relogin\" 出现 %d 次，want 2（接码 + 浏览器）", n)
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
		`st === "invalid" || st === "expired" || st === "unauthorized"`,
		`f.expired === true`,
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

// TestReloginDispatchesAndBailsOut: 有手机号会租号就走接码，否则走浏览器登录；
// 两样都没有要报错，而不是静默打开一个空弹层。
func TestReloginDispatchesAndBailsOut(t *testing.T) {
	src := reloginSource(t)
	body := poolStatsFuncBody(t, src, "openRelogin")

	if !strings.Contains(body, "const phoneAcct = /^\\d{6,15}$/.test(phone);") {
		t.Errorf("openRelogin 不再把账号名当手机号来判定")
	}
	// 能自己跑完整个登录的模块（loomy）走自动重登，号码钉住、验证码也由模块代取。
	if !strings.Contains(body, "if (autoOn(n) && phoneAcct && !loginOn(n)) return openAutoRelogin(n, acc, phone);") {
		t.Errorf("openRelogin 不再把自动登录模块交给自动重登")
	}
	if !strings.Contains(body, "if (smsOn(n) && phoneAcct) return openSMSRelogin(n, acc, phone);") {
		t.Errorf("openRelogin 不再把手机号账号交给接码重登")
	}
	if !strings.Contains(body, "if (loginOn(n)) return openBrowserRelogin(n, acc);") {
		t.Errorf("openRelogin 不再把其它账号交给浏览器重登")
	}
	if !strings.Contains(body, `toast(n + " 的账号既不能用接码重登，也没有浏览器登录，无法在面板里重新登录", "err")`) {
		t.Errorf("openRelogin 在两条路都不通时既不报错也不返回")
	}
	if !strings.Contains(src, `function loginOn(n) { return !!(capsOf(n || $("#addClient").value) || {}).login; }`) {
		t.Errorf("index.html 丢了 loginOn，openRelogin 的分支会直接抛错")
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

// reloginSource 是这些检查共用的 index.html 正文。
func reloginSource(t *testing.T) string {
	t.Helper()
	return string(indexHTML)
}

// TestAddDialogTabFollowsCapabilities: 「浏览器登录」那个 tab 其实是「不导入、不手填」
// 的入口，自动添加和接码都挂在它下面。只实现 AutoLoginProvider/SMSProvider 的模块
// （loomy）必须落在这个 tab 上并改名为「自动登录」，而不是被推进「手动添加」；三块
// 都没有的模块（minimaxcode）则要把这个 tab 藏掉，免得出现一个点开是空的按钮。
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
	if !strings.Contains(body, `if (!canLoginTab) { if (loginTab) loginTab.classList.remove("on"); setAddTab(k.import ? "import" : "manual"); }`) {
		t.Errorf("pickAddClient 在没有登录能力时不再退回导入/手动 tab")
	}
}
