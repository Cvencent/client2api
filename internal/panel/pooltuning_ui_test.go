package panel

import (
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 平台配置页 WorkBuddy 卡片里「账号池与流量治理」的回归测试。
//
// 这一盒的价值全在「改一个数字，池子的行为真的变了」上。而它要穿过四层：
//
//	index.html 的输入框 → 配置文件里的键 → cmd/client2api 读进 Go 结构
//	→ core.PoolTuning / core.LiveSettings → clients/workbuddy 的 applyPoolTuning
//
// 四层里任何一层漏掉，界面上都能正常输入、正常保存、正常回显，然后什么都不
// 发生——这正是本仓库反复出现的「死配置」缺陷形态。Go 编译器和 shell_test.go
// 的 id 守卫都看不见它（前者不管字符串，后者只保证 $() 指的 id 存在），所以
// 这里逐层静态钉住。
// ---------------------------------------------------------------------------

// poolTuningField 是 WorkBuddy 卡片里每一个可编辑的键，连同它在这四层里的
// 四个名字。
//
//	el       index.html 里 input 的 id
//	section  fileConfig 里的顶层节（pool / cooldown）
//	key      配置文件里的 JSON 键
//	goField  对应的 Go 字段名
//	tuning   true = 经 core.PoolTuning 进池；false = 经 core.LiveSettings 直供
type poolTuningField struct {
	el      string
	section string
	key     string
	goField string
	tuning  bool
}

var poolTuningFields = []poolTuningField{
	{"cfgPoolMaxInFlight", "pool", "max_in_flight", "MaxInFlight", false},
	{"cfgPoolMaxInFlightGlobal", "pool", "max_in_flight_global", "MaxInFlightGlobal", false},
	{"cfgPoolBreakerThreshold", "pool", "breaker_threshold", "BreakerThreshold", true},
	{"cfgCoolSoftRate", "cooldown", "soft_rate", "SoftRate", false},
	{"cfgCoolSoftRateMax", "cooldown", "soft_rate_max", "SoftRateMax", true},
	{"cfgPoolBreakerCooldown", "pool", "breaker_cooldown", "BreakerCooldown", true},
	{"cfgPoolBreakerCooldownMax", "pool", "breaker_cooldown_max", "BreakerCooldownMax", true},
	{"cfgPoolDegradeThreshold", "pool", "degrade_threshold", "DegradeThreshold", true},
	{"cfgPoolDegradeCooldown", "pool", "degrade_cooldown", "DegradeCooldown", true},
	{"cfgPoolDegradeCooldownMax", "pool", "degrade_cooldown_max", "DegradeCooldownMax", true},
	{"cfgPoolIdlePerHour", "pool", "idle_weight_per_hour", "IdleWeightPerHour", true},
	{"cfgPoolIdleMax", "pool", "idle_weight_max", "IdleWeightMax", true},
	{"cfgPoolCostExplore", "pool", "cost_explore_interval", "CostExploreInterval", true},
	{"cfgPoolExpiringSoon", "pool", "expiring_soon", "ExpiringSoon", false},
	{"cfgPoolPreferExpiring", "pool", "prefer_expiring", "PreferExpiring", true},
}

// TestWorkBuddyCardRendersAndSavesEveryPoolTuningKnob 钉住第一、二层：
// 每个键都只能在 WorkBuddy 平台卡片里渲染，并且在 savePlatforms 里被读走
// （组装 patch）。「渲染了但没保存」是这一盒最容易犯的错——输入框看着好好的，
// 按保存无效。
func TestWorkBuddyCardRendersAndSavesEveryPoolTuningKnob(t *testing.T) {
	src := poolStatsUISource(t)
	host := poolStatsFuncBody(t, src, "renderPlatforms")
	render := poolStatsFuncBody(t, src, "pfWorkBuddyPoolHTML")
	save := poolStatsFuncBody(t, src, "savePlatforms")

	if !strings.Contains(host, `if (n === "workbuddy") out.push(pfWorkBuddyPoolHTML(c))`) {
		t.Error("renderPlatforms 没有把号池设置放进 WorkBuddy 卡片")
	}
	if !strings.Contains(host, `push("workbuddy")`) {
		t.Error("renderPlatforms 没有保证 WorkBuddy 卡片始终存在：模块停用后号池设置会失联")
	}
	for _, f := range poolTuningFields {
		rendered := false
		for _, h := range []string{`pfWBText("`, `pfWBNum("`, `pfWBCheck("`} {
			if strings.Contains(render, h+f.el+`"`) {
				rendered = true
				break
			}
		}
		if !rendered {
			t.Errorf("WorkBuddy 卡片没有渲染 %s：这个键操作员改不到", f.el)
		}
		// 数字/时长行把整条选择器交给 helper（num("id", …)），开关行才直接
		// 查表（$("#id")）。三种写法都算「读了这个输入框」。
		used := false
		for _, h := range []string{`$("#`, `readNumField("`, `readDurField("`} {
			if strings.Contains(save, h+f.el+`"`) {
				used = true
				break
			}
		}
		if !used {
			t.Errorf("savePlatforms 没有读 %s：输入框能改，但保存时被丢掉", f.el)
		}
	}

	// 两节都要真的写进同一个 PATCH。漏掉某一节，那一节的输入框全是摆设。
	for _, want := range []string{"patch.platforms = changed", "patch.pool = pool", "patch.cooldown = cool"} {
		if !strings.Contains(save, want) {
			t.Errorf("savePlatforms 里找不到 %q：这一节的输入框保存不到磁盘", want)
		}
	}
	// prefer_expiring 是 *bool：勾与不勾都要能回写，否则勾掉之后关不掉。
	if !strings.Contains(save, `pool.prefer_expiring = now`) {
		t.Error("prefer_expiring 没有回写：取消勾选后关不掉这个路由")
	}
	// 空串 = 删键回默认，但 0 是合法值（max_in_flight 的 0 = 不限制，
	// cost_explore_interval 的 0 = 关停探索）。把 0 当空会把它们抹成默认值。
	read := poolStatsFuncBody(t, src, "readNumField")
	if !strings.Contains(read, `if (raw === "") { if (key in base) obj[key] = null; return true; }`) {
		t.Error("留空回默认的判据变了：必须只认空字符串，不能把 0 当空")
	}
}

// Session sticky is global rather than WorkBuddy-specific, so its TTL stays on
// the gateway config page even though the pool controls moved into a platform.
func TestGlobalConfigStillEditsSessionStickyTTL(t *testing.T) {
	src := poolStatsUISource(t)
	render := poolStatsFuncBody(t, src, "renderConfig")
	save := poolStatsFuncBody(t, src, "saveConfig")

	if !strings.Contains(render, `cfgText("cfgStickyTTL"`) {
		t.Error("网关配置页没有渲染全局 session_sticky.ttl")
	}
	if !strings.Contains(save, `readDurField("cfgStickyTTL", "ttl", sticky, bSticky)`) || !strings.Contains(save, `patch.session_sticky = sticky`) {
		t.Error("saveConfig 没有保存全局 session_sticky.ttl")
	}
}

// TestEveryPoolTuningKnobReachesTheConfigStruct 钉住第二、三层：
// 界面写下的键必须真的是 fileConfig 里的字段，而且被 main.go 读进 Go。
// JSON tag 拼错一个字母，保存会成功（深合并把未知键原样写盘），然后永远不生效。
func TestEveryPoolTuningKnobReachesTheConfigStruct(t *testing.T) {
	const mainGo = "../../cmd/client2api/main.go"
	b, err := os.ReadFile(mainGo)
	if err != nil {
		t.Skipf("cannot read %s: %v", mainGo, err)
	}
	src := string(b)

	section := map[string]string{
		"pool":     "Pool",
		"cooldown": "Cooldown",
	}
	for _, f := range poolTuningFields {
		if !strings.Contains(src, `json:"`+f.key+`"`) {
			t.Errorf("%s 里没有 json tag %q：界面上保存的 %s 会被当成未知键原样写盘，永远不生效",
				mainGo, f.key, f.el)
		}
		// 接收者可能是 c（applyDefaults / poolTuning）或 cfg（main），
		// 所以只钉「节.字段」这一段。
		ref := "." + section[f.section] + "." + f.goField
		if !strings.Contains(src, ref) {
			t.Errorf("%s 里没有 %s：fileConfig 声明了这个键，但没人读它（死配置）", mainGo, ref)
		}
	}
}

// goFuncBody 截取 Go 源码里一个函数的函数体：从 " name(" 起，到下一个独占一行
// 的 "}" 止（嵌套块的收尾大括号都是缩进的，所以顶格的那个就是函数尾）。
// poolStatsFuncBody 找的是 JS 的 "function name("，对 Go 源码永远匹配不上。
func goFuncBody(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, " "+name+"(")
	if start < 0 {
		t.Fatalf("no function %s", name)
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("function %s looks unterminated", name)
	}
	return rest[:end]
}

// TestEveryPoolTuningKnobReachesThePool 钉住第四层：读进 Go 的值必须真的被
// 交给池。core.PoolTuning 声明了字段、applyPoolTuning 却没读，是这一层最隐蔽
// 的失败——编译通过、启动日志照打，池子毫不知情。
func TestEveryPoolTuningKnobReachesThePool(t *testing.T) {
	tuning, err := os.ReadFile("../core/liveapply.go")
	if err != nil {
		t.Skipf("cannot read ../core/liveapply.go: %v", err)
	}
	const policyGo = "../../clients/workbuddy/poolpolicy.go"
	policy, err := os.ReadFile(policyGo)
	if err != nil {
		t.Skipf("cannot read %s: %v", policyGo, err)
	}
	body := goFuncBody(t, string(policy), "applyPoolTuning")
	if strings.TrimSpace(body) == "" {
		t.Fatal("poolpolicy.go 里没有 applyPoolTuning")
	}

	for _, f := range poolTuningFields {
		if !f.tuning {
			continue
		}
		if !strings.Contains(string(tuning), f.goField) {
			t.Errorf("core.PoolTuning 没有字段 %s：%s 保存的值到不了池子", f.goField, f.el)
		}
		if !strings.Contains(body, "t."+f.goField) {
			t.Errorf("applyPoolTuning 没有读 t.%s：%s 保存的值在最后一跳被丢掉", f.goField, f.el)
		}
	}
}

// TestPoolTuningDefaultsMatchTheReference 钉住两处曾经写错的默认值。它们不
// 是文案：expiring_soon 的 1h 窗口小到几乎没有积分够格，cost_explore_interval
// 的 10m 则是参考值的三分之一（探索频率×3，而探索是搭车改道，只会多花钱）。
func TestPoolTuningDefaultsMatchTheReference(t *testing.T) {
	b, err := os.ReadFile("../../cmd/client2api/main.go")
	if err != nil {
		t.Skipf("cannot read ../../cmd/client2api/main.go: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		`c.Pool.ExpiringSoon = "168h"`,
		`c.Pool.CostExploreInterval = "30m"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("applyDefaults 里找不到 %q：默认值偏离了参考实现", want)
		}
	}
}
