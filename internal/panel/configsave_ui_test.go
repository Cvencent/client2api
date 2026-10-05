package panel

import (
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 「保存配置」必须只写操作员真正改过的东西
// ---------------------------------------------------------------------------
//
// 配置页渲染的是**生效值**，不是文件里写了什么：缺键时排程组开关、
// session_sticky.enabled、sanitize_blacklist_fingerprints 都画成「开」，
// panel.package_detail_limit 画的是服务端解析后的默认值，没有配置节的模块
// 画成 {}。而 saveConfig 是「表单形状」的：它把看得见的每个字段都塞进 patch。
//
// 两边单独看都对，合起来就是：点一次「保存配置」、什么都不改，文件里会凭空
// 多出 7 个空模块节和 6 个 schedule.*_enabled，外加 pool.prefer_expiring、
// session_sticky.enabled、panel.package_detail_limit，显式写成空串的
// prompt.file 还会被删掉；然后页面如实报告「已保存 clients、panel、pool、
// prompt、schedule、session_sticky（重启后生效）」——如实报告的是一批操作员
// 从没做过的改动，还附带一句「请重启进程」。
//
// 修法是渲染时记一份基线（CFG.baseline），保存前剪掉跟基线相同的字段。
// 这个测试守两件事：①每个「缺键画默认值」的控件都在基线里登记了（漏一个就
// 会退回幻象改动）；②saveConfig 真的剪了，而且剪空了不发请求（后端对空
// patch 是 400，页面该说的是「内容没有变化」）。
//
// 真正端到端的验证在 Edge 套件里（run/panelsuite2.js 的「原样保存一次不改变
// 磁盘内容」）：它真的点保存、真的 diff 磁盘文件。这里只能守形状。
func TestSaveConfigSendsOnlyWhatTheOperatorChanged(t *testing.T) {
	ui := poolStatsUISource(t)
	render := poolStatsFuncBody(t, ui, "renderConfig")
	save := poolStatsFuncBody(t, ui, "saveConfig")
	prune := poolStatsFuncBody(t, ui, "pruneUnchanged")

	// 自动排程已迁到任务中心：配置页不再渲染这一块，否则两边会各写一半。
	if strings.Contains(render, "cfgSchEnabled") || strings.Contains(render, "自动排程") {
		t.Error("配置页又渲染了自动排程块：它已经迁到任务中心，重复的入口会互相覆盖")
	}

	// ① 基线登记齐了。画默认值的每一处都得有对应登记，漏掉一处就退回幻象改动。
	for _, site := range []string{
		"sticky.enabled == null",                  // 会话粘性
		"sanitize_blacklist_fingerprints == null", // 出站指纹清洗
		"pnl.package_detail_limit != null ? pnl.package_detail_limit : pnlEff.package_detail_limit", // 面板
		"cl[n] == null ? {} : cl[n]", // 模块配置节
	} {
		if !strings.Contains(render, site) {
			t.Errorf("renderConfig 里找不到默认值渲染点 %q：基线是按这些点登记的，改动后要同步", site)
		}
	}
	if !strings.Contains(render, "CFG.baseline = baseline") {
		t.Error("renderConfig 没有把渲染基线记到 CFG.baseline")
	}
	for _, path := range []string{
		`"session_sticky.enabled"`,
		`"features.sanitize_blacklist_fingerprints"`,
		`"panel.package_detail_limit"`,
	} {
		if !strings.Contains(render, "setBase("+path) {
			t.Errorf("渲染基线漏了 %s：这个控件缺键时会画默认值，保存时会被当成一次改动", path)
		}
	}
	// pool.prefer_expiring 的输入框在 0.1.13 就搬去了「平台配置 · WorkBuddy 卡片」，
	// savePlatforms 用 PF.poolBaseline 自己剪枝，配置页不再渲染它。这条 setBase 却
	// 一直留在 renderConfig 里，而 pool 只在 pfWorkBuddyPoolHTML / savePlatforms 里
	// 声明过 —— renderConfig 一跑到就抛 ReferenceError，整张配置页白屏，保存按钮
	// 也点不到。这条断言就是钉住它别被搬回来。
	if codeRefs(render, "pool.prefer_expiring") {
		t.Error("renderConfig 又引用了 pool.prefer_expiring：这个输入框早已搬去平台配置页，本函数里根本没有 pool 变量，引用它会让整张配置页抛 ReferenceError 白屏")
	}
	if !strings.Contains(render, `setBase("clients." + n, cl[n] == null ? {} : cl[n])`) {
		t.Error("渲染基线漏了模块配置节：缺失的节画成 {}，保存时会凭空多出空节")
	}

	// ② 保存前剪枝，剪空不发请求。
	if !strings.Contains(save, "pruneUnchanged(patch, CFG.baseline") {
		t.Error("saveConfig 没有拿渲染基线剪掉没变的字段")
	}
	if !strings.Contains(save, "body: pruned") {
		t.Error("saveConfig 没有把剪枝后的 patch 发出去")
	}
	if strings.Contains(save, "body: patch") {
		t.Error("saveConfig 仍然在发送未剪枝的 patch")
	}
	if !strings.Contains(save, "if (!Object.keys(pruned).length)") {
		t.Error("saveConfig 剪空之后仍然会发请求（后端会以 empty patch 400 拒绝）")
	}

	// ②b 模块配置节里的 {} 必须归一成 null（删节 = 用模块默认值）。面板把没有
	// 配置节的模块画成 {}，操作员「清空这一节」的自然写法就是留下一个 {}；可
	// mergeConfig 是递归合并，{} 合进一个非空节等于什么都没做——面板画着 {}
	// 说「这就是默认」，保存却对已有的节毫无作用，操作员会以为重置成功了。
	// 断整行而不是几个片段：片段式断言连 `if (false && …)` 这种被关掉的
	// 分支都认，等于没守。
	const norm = `if (c && typeof c === "object" && !Array.isArray(c) && !Object.keys(c).length) clients[name] = null;`
	if !strings.Contains(save, norm) {
		t.Errorf("saveConfig 没有把空对象形式的模块配置节归一成 null：\n想要 %s\n对着有内容的节写 {} 会被 mergeConfig 静默吃掉，操作员会以为重置成功了", norm)
	}
	// 帮助文案也得说实话，否则操作员仍会以为 {} 能重置。
	if !strings.Contains(ui, "留空或写 <code>{}</code> 都表示删除该模块的配置节") {
		t.Error("模块配置节的帮助文案还在说「{} 表示全部使用模块默认值」——那是假的，写 {} 不会重置一节")
	}

	// ③ 剪枝规则本身。null 是「删键」，删一个本来就空/不存在的键不是改动；
	// 对象递归剪，剪空就丢——空对象合进磁盘上已有的节本来就是空操作。
	for _, want := range []string{
		`const wasEmpty = !has || bv === null || bv === "" || (isObj(bv) && !Object.keys(bv).length);`,
		`if (!wasEmpty) out[k] = null;`,
		`const sub = isObj(bv) ? pruneUnchanged(pv, bv) : pv;`,
		`if (has && same(pv, bv)) continue;`,
	} {
		if !strings.Contains(prune, want) {
			t.Errorf("pruneUnchanged 缺少规则 %q", want)
		}
	}
	if strings.Contains(prune, "out[k] = {}") {
		t.Error("pruneUnchanged 又把「清成 {}」当成一次重置了：空对象合进非空节是空操作，发出去只会换来一句「已保存」而磁盘不动")
	}
}

// codeRefs reports whether body mentions name in actual code, ignoring comments.
// Static "does the script still reference X" checks are only meaningful if an
// explanatory comment about X cannot pass for a real reference; without this,
// documenting the bug you just fixed immediately re-breaks the test.
func codeRefs(body, name string) bool {
	body = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(body, " ")
	body = regexp.MustCompile(`(?m)//[^\n]*`).ReplaceAllString(body, " ")
	return strings.Contains(body, name)
}
