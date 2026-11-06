package panel

import (
	"sort"
	"strings"
	"testing"
)

// 钉住「动效偏好的枚举完整性」（issue #222）。
//
// 背景：`initSpotlight` 是全族唯一不判 `motionReduce()` 的初始化函数——同族的
// initMagnet / initTilt / initRipple / initPressFx / initSpark 都判（前四个在
// 使用点、initSpark 在建期）。它漏判的后果是：reduce 打开后，覆盖面最广的
// `.box` 光斑仍然每帧跟着指针走（圆心由 CSS 变量逐帧重写，`transition:none`
// 关不掉）。
//
// 为什么这里用静态枚举而不是浏览器门禁：先写过一版 `cmd/panelsmoke` 阶段，
// 在「有守卫」与「临时移除守卫」两种构建上**都是绿的**——harness 里合成的
// pointermove 打不到 `window` 上的监听器，那个阶段从来没走到被测路径。假绿的
// 门禁比没有门禁更危险，所以撤掉了它，改用能真正判定的形式：
//
//	① 初始化函数名单里，凡挂 pointermove 的必须判偏好（枚举完整性）；
//	② `initSpotlight` 必须判（本条的具体落点）。
//
// 这不是「文本里有」式的弱断言：它枚举的是**函数集合**，任何新增的指针驱动
// 动效都会自动进名单，漏判即红。
func TestPointerDrivenEffectsRespectMotionReduce(t *testing.T) {
	src := poolStatsUISource(t)

	// 枚举：所有 init* 初始化函数体。
	names := initFunctionNames(src)
	if len(names) < 5 {
		t.Fatalf("只找到 %d 个 init* 函数（%v）：枚举的手失去锚点", len(names), names)
	}
	if !containsName(names, "initSpotlight") {
		t.Fatal("initSpotlight 不在枚举里：锚点失效")
	}

	var offenders []string
	for _, fn := range names {
		body := poolStatsFuncBody(t, src, fn)
		if !strings.Contains(body, "pointermove") {
			continue // 非指针驱动，不适用
		}
		if strings.Contains(body, "motionReduce()") {
			continue
		}
		// 允许「在创建期判偏好」的形态：整个函数在建之前就 return。
		if strings.Contains(body, "prefers-reduced-motion") {
			continue
		}
		offenders = append(offenders, fn)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("这些指针驱动的动效没有判 prefers-reduced-motion：%v\n"+
			"reduce 打开时它们仍会跟随指针（issue #222）。照 initMagnet 的写法在"+
			"pointermove 回调开头加 `if (motionReduce()) return;`——判定在使用点还"+
			"天然满足 #205 的「离开 reduce 后能自己回来」，不需要重建任何东西。", offenders)
	}
}

// containsName reports whether fn is in names.
func containsName(names []string, fn string) bool {
	for _, n := range names {
		if n == fn {
			return true
		}
	}
	return false
}

// initFunctionNames lists the `function initXxx()` declarations in src.
func initFunctionNames(src string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; ; {
		j := strings.Index(src[i:], "\nfunction init")
		if j < 0 {
			break
		}
		j += i + len("\nfunction ")
		k := strings.Index(src[j:], "(")
		if k < 0 {
			break
		}
		name := src[j : j+k]
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		i = j + 1
	}
	sort.Strings(out)
	return out
}
