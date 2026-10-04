package panel

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 配置页「自动排程」区与看板「一次性批量」按钮的回归测试。
//
// 这两块界面都只是 index.html 里的字符串拼接，而且 Go 侧本来就已经齐了
// （configwrite.go 的 mergeConfig 是通用 map 合并、接受任意顶层键；
// panel.go 的 /panel/api/status 已经带 schedule），所以没有任何编译期检查能拦住
// 「表单少写一个键」「按钮画在了没有能力的模块上」「改名后 $() 指向空气」这类
// 退化——shell_test.go 只保证 id 对得上，不保证这些。这里静态钉住三件事：
//   1. 自动排程区读写的键 == cmd/client2api 的 scheduleConfig 的 json tag；
//   2. scheduler.Status 的三态（没接调度器 / 总开关关着 / 在跑）都有渲染分支；
//   3. 批量按钮的 verb 与 batches.go 的别名表一一对应，且被 caps.batches 门控。
// ---------------------------------------------------------------------------

// scheduleFormKeys 是自动排程区读写的全部键，顺序即界面顺序。
// 与 cmd/client2api/main.go 的 scheduleConfig（json tag）必须完全一致。
var scheduleFormKeys = []string{
	"enabled",
	"checkin_hours", "checkin_enabled",
	"keepalive_hours", "keepalive_enabled",
	"travel_hours", "travel_enabled",
	"activity_hours", "activity_enabled",
	"blackcat_hours", "blackcat_enabled",
	"balance_refresh_enabled", "balance_refresh_minutes",
	"growth_hours", "growth_enabled",
	// clients 不在这张表单里：按平台的自定义时点编辑器在任务中心
	// （#view-taskscenter 的「定时任务」盒），但写的是同一个 schedule.* 节，
	// 所以这个键仍然属于这一块。
	"clients",
}

// scheduleFormIDs 是自动排程区声明的元素 id；后半段是 saveConfig 要用 $()
// 逐个读回来的输入控件。
var scheduleFormIDs = []string{
	"cfgSchState", "cfgSchNext",
	"cfgSchEnabled",
	"cfgSchCheckinEnabled", "cfgSchCheckinHours",
	"cfgSchKeepaliveEnabled", "cfgSchKeepaliveHours",
	"cfgSchTravelEnabled", "cfgSchTravelHours",
	"cfgSchActivityEnabled", "cfgSchActivityHours",
	"cfgSchBlackcatEnabled", "cfgSchBlackcatHours",
	"cfgSchGrowthEnabled", "cfgSchGrowthHours",
	"cfgSchBalanceEnabled", "cfgSchBalanceMinutes",
}

func scheduleShellSource(t *testing.T) string {
	t.Helper()
	src := string(indexHTML)
	if strings.TrimSpace(src) == "" {
		t.Fatal("index.html was not embedded")
	}
	return src
}

// scheduleFuncBody 截取一个顶层 function 的函数体：这些函数都以行首的 "}" 结束，
// 所以到下一个 "\n}" 为止就是完整实现（够用来断言"这段代码里有没有这个判断"）。
func scheduleFuncBody(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "function "+name+"(")
	if start < 0 {
		t.Fatalf("index.html has no function %s", name)
	}
	rest := src[start:]
	if end := strings.Index(rest, "\n}"); end >= 0 {
		return rest[:end]
	}
	t.Fatalf("function %s looks unterminated", name)
	return ""
}

// 一个 id 只要满足 shell_test.go 的两条"定义"规则之一，守卫就认它。
func scheduleIDDeclared(src, id string) bool {
	return strings.Contains(src, `id="`+id+`"`) || strings.Contains(src, `cfgText("`+id+`"`)
}

func TestScheduleFormIDsAreDeclaredAndRead(t *testing.T) {
	src := scheduleShellSource(t)
	var declareOnly []string
	for _, id := range scheduleFormIDs {
		if !scheduleIDDeclared(src, id) {
			t.Errorf("自动排程区的 %s 没有任何 id= 或 cfgText() 声明（shell id 守卫会漏，因为没人引用它）", id)
		}
		if id == "cfgSchState" || id == "cfgSchNext" {
			declareOnly = append(declareOnly, id)
			continue
		}
		// 输入控件必须真的被 saveConfig 读回去，否则"画了框但不保存"。
		if !strings.Contains(src, `$("#`+id+`")`) {
			t.Errorf("自动排程区的 %s 声明了却没人 $() 读它：保存时这个字段会被丢掉", id)
		}
	}
	if len(declareOnly) != 2 {
		t.Fatalf("状态条应当只有两个只读元素（cfgSchState/cfgSchNext），实际算了 %d 个", len(declareOnly))
	}
}

// TestScheduleKeysMatchTheConfigStruct 是本文件里唯一跨包的一致性检查：界面写的
// 键名必须逐字等于 Go 侧读的 json tag（多一个键 = 静默丢一个设置，
// 少一个键 = 面板永远改不动它）。cmd/client2api/main.go 被并发改成别的样子时
// 这里会失败，那正是要人来看一眼的时候。
func TestScheduleKeysMatchTheConfigStruct(t *testing.T) {
	const path = "../../cmd/client2api/main.go"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cannot read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)type scheduleConfig struct \{.*?\n\}`).Find(b)
	if block == nil {
		t.Fatalf("%s 里找不到 type scheduleConfig struct", path)
	}
	inGo := map[string]bool{}
	for _, m := range regexp.MustCompile(`json:"([a-z_]+)"`).FindAllSubmatch(block, -1) {
		inGo[string(m[1])] = true
	}
	if len(inGo) == 0 {
		t.Fatalf("%s 的 scheduleConfig 里一个 json tag 都没解析到", path)
	}
	inUI := map[string]bool{}
	for _, k := range scheduleFormKeys {
		if inUI[k] {
			t.Errorf("scheduleFormKeys 里 %q 重复了", k)
		}
		inUI[k] = true
		if !inGo[k] {
			t.Errorf("面板读写 schedule.%s，但 Go 的 scheduleConfig 没有这个 json tag", k)
		}
	}
	for k := range inGo {
		if !inUI[k] {
			t.Errorf("Go 的 scheduleConfig 有 %q，面板没有对应的输入框", k)
		}
	}
}

// TestScheduleStatusRendersTheThreeStates 钉住三态：status 里没有 schedule 字段
// （没接调度器）/ enabled:false（接了但关着）/ enabled:true（在跑，列下一跳）。
// 三态在 Status 上是三种不同的形状，混成一种会给操作员错误的信息。
func TestScheduleStatusRendersTheThreeStates(t *testing.T) {
	src := scheduleShellSource(t)
	for _, want := range []string{"没有接入调度器", "总开关是关的", "调度器在跑"} {
		if !strings.Contains(src, want) {
			t.Errorf("状态条缺少 %q 这一态", want)
		}
	}
	// 字段来自 GET /panel/api/status 的 schedule（overview 里没有），三态是三条分支。
	for _, want := range []string{
		`api("/panel/api/status")`,
		`if (sched == null) {`,
		`if (!sched.enabled) {`,
		`sched.next`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("状态条里找不到 %q：三态渲染不完整", want)
		}
	}
}

// TestBatchVerbsMatchTheAliasTable 让界面上的按钮和面板实际接受的路由绑在一起：
// 每个按钮 POST 的是 <verb>_all，别名表在 batches.go，两边错一个字就是 404。
func TestBatchVerbsMatchTheAliasTable(t *testing.T) {
	src := scheduleShellSource(t)
	found := map[string]bool{}
	for _, m := range regexp.MustCompile(`\{ verb: "([a-z_]+)"`).FindAllStringSubmatch(src, -1) {
		found[m[1]] = true
	}
	if len(found) == 0 {
		t.Fatal("index.html 里解析不到 BATCH_VERBS")
	}
	for alias := range allVerbs {
		name, ok := batchNameFromAllVerb(alias)
		if !ok {
			t.Errorf("batches.go 的别名 %q 自己都解不出 batch 名", alias)
			continue
		}
		if alias != name+"_all" {
			t.Errorf("别名 %q 解出来的是 batch %q，不是 %q_all", alias, name, name)
		}
		if !found[name] {
			t.Errorf("路由支持 %q（batch %q），但面板没有画这个按钮", alias, name)
		}
	}
	for verb := range found {
		if _, ok := allVerbs[verb+"_all"]; !ok {
			t.Errorf("面板会 POST %s_all，但 batches.go 的 allVerbs 里没有 %s_all", verb, verb)
		}
	}
}

// TestBatchToastsAreTheReferenceOnes 钉住原版逐字文案。它是回归，不是文案建议：
// 运维按这四条去日志里找东西，改了就对不上了。
func TestBatchToastsAreTheReferenceOnes(t *testing.T) {
	src := scheduleShellSource(t)
	for _, want := range []string{
		"全部签到已开始，结果见日志",
		"全部保活已开始，结果见日志",
		"旅行巡检已开始（含领养链路），结果见日志",
		"活跃上报已开始，结果见日志",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("缺少原版逐字 toast %q", want)
		}
	}
}

// TestBatchButtonsAreGatedOnTheCapabilityBit 是本块界面的关键不变量：caps.batches
// 为假 = 模块没实现 core.BatchPlanner，POST 过去只会拿到 501，所以按钮必须不画。
// 如果哪天有人为了"看得见"把门控删掉，这个测试会红。
func TestBatchButtonsAreGatedOnTheCapabilityBit(t *testing.T) {
	src := scheduleShellSource(t)
	if !strings.Contains(src, `id="qcBatchBar" hidden`) {
		t.Error("批量条必须默认 hidden：能力位要靠 renderQCBatches 才打开")
	}
	body := scheduleFuncBody(t, src, "renderQCBatches")
	for _, want := range []string{`capsOf(n).batches`, `bar.hidden = true`, `bar.hidden = false`} {
		if !strings.Contains(body, want) {
			t.Errorf("renderQCBatches 里找不到 %q：按钮不再受 caps.batches 门控", want)
		}
	}
	// 门控必须在显示之前（先判能力再画按钮），且请求形状是 <client>/<verb>_all。
	if strings.Index(body, "capsOf(n).batches") > strings.Index(body, "bar.hidden = false") {
		t.Error("renderQCBatches 先显示后判能力位：没有能力的模块会看到点不动的按钮")
	}
	run := scheduleFuncBody(t, src, "qcBatchRun")
	if !strings.Contains(run, `cbase(n) + "/" + entry.verb + "_all"`) {
		t.Error(`qcBatchRun 没有按 <client>/<verb>_all 的形状 POST`)
	}
	// 非 2xx（501 没实现 / 409 已有 sweep 在跑）必须原样端服务端文案。
	if !strings.Contains(run, "toast(r.err") {
		t.Error("qcBatchRun 把服务端错误吃掉了：501/409 应当原样显示")
	}
}

// ---------------------------------------------------------------------------
// 帮助函数自身的行为（它们在浏览器里跑，Go 侧只能拿源码做静态检查；这里至少把
// 两个容易写错的语义钉住：hours 的解析规则和"缺键 = 开"的默认值）。
// ---------------------------------------------------------------------------

func TestScheduleHoursParsingRules(t *testing.T) {
	// parseSchedHours 的语义：空 = 空数组（这组永不跑），非法 = null（拒绝保存）。
	// 这里用同一段正则的可见性做最低限度的确认：函数存在、且 0 和 23 是边界。
	src := scheduleShellSource(t)
	body := scheduleFuncBody(t, src, "parseSchedHours")
	for _, want := range []string{"n < 0", "n > 23", "Number.isInteger(n)"} {
		if !strings.Contains(body, want) {
			t.Errorf("parseSchedHours 缺少 %q", want)
		}
	}
	if !strings.Contains(src, "const schedChecked = v => (v === undefined || v === null || !!v) ? \" checked\" : \"\";") {
		t.Error("schedChecked 不再把「文件里没有这个键」当成勾选：Go 侧组开关缺省是开，不改会静默关掉默认开着的组")
	}
}

// 一个反例：把 cfgSchEnabled 从源码里挖掉，两个检查都要发现它。没有这个负控，
// 上面那堆 strings.Contains 可能是"总能匹配到"的自证。
func TestScheduleIDChecksCatchAMissingField(t *testing.T) {
	src := scheduleShellSource(t)
	broken := strings.Replace(src, `$("#cfgSchEnabled")`, `$("#cfgSchRenamed")`, -1)
	if broken == src {
		t.Fatal("测试自己失效了：源码里没有可替换的 $(\"#cfgSchEnabled\")")
	}
	var dangling []string
	declared := map[string]bool{}
	for _, re := range []*regexp.Regexp{shellIDAttr, shellMinted} {
		for _, m := range re.FindAllStringSubmatch(broken, -1) {
			declared[m[1]] = true
		}
	}
	if declared["cfgSchRenamed"] {
		t.Error("改名后的 id 不该还算已声明")
	}
	for _, re := range shellRefs {
		for _, m := range re.FindAllStringSubmatch(broken, -1) {
			if !declared[m[1]] {
				dangling = append(dangling, m[1])
			}
		}
	}
	sort.Strings(dangling)
	if len(dangling) == 0 {
		t.Error("shell id 守卫对改名后的控件没意见：它守不住这一块界面")
	}
}

// ---------------------------------------------------------------------------
// 任务中心的「定时任务」盒。
//
// 它不另开一份配置：时点仍然读写 schedule.*，只是把「每个平台各自几点」
// 画成了一行，并加了一个手动触发按钮。这里的检查就是钉住这三件事。
// ---------------------------------------------------------------------------

// TestScheduleOverrideKeysMatchTheConfigStruct 是本文件里第二个跨包检查：
// 面板写的 schedule.clients.<平台>.<任务> 的两个子键必须逐字等于 Go 侧
// scheduleOverride 的 json tag；错一个就是「保存成功、热加载当作没写」。
func TestScheduleOverrideKeysMatchTheConfigStruct(t *testing.T) {
	const path = "../../cmd/client2api/main.go"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cannot read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)type scheduleOverride struct \{.*?\n\}`).Find(b)
	if block == nil {
		t.Fatalf("%s 里找不到 type scheduleOverride struct", path)
	}
	got := map[string]bool{}
	for _, m := range regexp.MustCompile(`json:"([a-z_]+)"`).FindAllSubmatch(block, -1) {
		got[string(m[1])] = true
	}
	for _, want := range []string{"enabled", "hours"} {
		if !got[want] {
			t.Errorf("scheduleOverride 缺少 json tag %q：面板写的这份自定义会被丢掉", want)
		}
	}
	if len(got) != 2 {
		t.Errorf("scheduleOverride 的 json tag = %v, want exactly enabled/hours", got)
	}
}

// TestTasksCenterScheduleEditorReadsAndWritesTheRightShape 钉住界面这一侧：
// 读 /panel/api/schedule 一次，写 schedule.clients.<平台>.<任务>，复用 reload 热
// 加载，并能把一行改回「跟随全局」（写 null 删掉自定义）。
func TestTasksCenterScheduleEditorReadsAndWritesTheRightShape(t *testing.T) {
	src := scheduleShellSource(t)
	for _, want := range []string{
		`api("/panel/api/schedule")`,
		`id="scBody"`,
		`id="scRunsBody"`,
		`data-scfollow`,
		`data-scrun`,
		`class="cfgtext scHours"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("任务中心的定时任务盒缺少 %q", want)
		}
	}
	// 立即执行必须复用既有的按批运行路由，而不是自创一条。
	if !strings.Contains(src, `"/batches/" + encodeURIComponent(batch) + "/run"`) {
		t.Error("立即执行没有走 <client>/batches/<batch>/run")
	}
	save := scheduleFuncBody(t, src, "scSave")
	for _, want := range []string{
		`clients[pend.client][pend.batch] = { enabled:`,
		`clients[rem.client][rem.batch] = null`,
		`api("/panel/api/config", { method: "PATCH", body: { schedule: sch } })`,
		`api("/panel/api/reload", { method: "POST" })`,
	} {
		if !strings.Contains(save, want) {
			t.Errorf("scSave 里找不到 %q", want)
		}
	}
	// 三态说明与配置页保持同一个词：没接调度器 / 总开关关着 / 在跑。
	state := scheduleFuncBody(t, src, "scStateHTML")
	for _, want := range []string{"没有接入调度器", "总开关是关的", "调度器在跑"} {
		if !strings.Contains(state, want) {
			t.Errorf("scStateHTML 缺少 %q 这一态", want)
		}
	}
}
