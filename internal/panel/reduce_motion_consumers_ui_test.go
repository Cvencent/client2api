package panel

import (
	"strings"
	"testing"
)

// 「偏好/状态通道已经存在，但有一个消费者从没被接上去」一类（issue #54）。
//
// 这条单的取证法不是看某块特效写没写，而是「把该偏好的全部消费者列出来，
// 看哪一个没订阅」：CSS 的 @media 立刻生效、WebGL 靠帧内 motionReduce() 分支
// 静止，唯独两块 2D canvas（背景粒子场、点击火花）没接——运行中从「关」切到
// 「开」之后，它们照旧动到下次刷新。看起来「已经生效」，比完全不支持更糟。
//
// 判据：常驻的 rAF 循环必须有对应的 stop 函数，而且 stop 必须被调用；
// 只在建立时查一次偏好的守卫，必须在使用点再查一次。

// TestReduceMotionChangeStopsTheBackgroundField 钉住 issue #54 的第一块：
// onReduceChange 必须在两个方向上都驱动 BGF 循环，而 bgfStop 不许再是死代码。
func TestReduceMotionChangeStopsTheBackgroundField(t *testing.T) {
	src := poolStatsUISource(t)

	// stop 函数必须存在。
	if !strings.Contains(src, "function bgfStop() {") {
		t.Fatal("bgfStop 不见了：背景粒子场没有停下来的入口（issue #54）")
	}
	// 它必须真的被调用过（此前全文件只有定义）。
	if !strings.Contains(src, "if (mql.matches) bgfStop();") {
		t.Fatal("onReduceChange 没有在进入 reduce 时调 bgfStop()：粒子场会一直漂移到下次刷新（issue #54）")
	}
	// 反向也要接：离开 reduce 得重新起循环，否则一旦切过一次就再也回不来。
	if !strings.Contains(src, "else if (typeof bgfStart === \"function\") bgfStart();") {
		t.Fatal("onReduceChange 没有在离开 reduce 时重新起循环：粒子场再也回不来了（issue #54）")
	}
	// 停下之后要清一帧：只 cancel 的话粒子连线会结在屏幕上。
	// 清画布的实现抽到了 bgfWipe（issue #91 把「从隐藏回到可见也要抹一帧」一起
	// 收进去了），所以这里钉「bgfStop 必须调它」+「bgfWipe 必须真的清」两段，
	// 而不是把 clearRect 钉在 bgfStop 的循环体里。
	stop := poolStatsFuncBody(t, src, "bgfStop")
	if !strings.Contains(stop, "bgfWipe(") {
		t.Error("bgfStop 没有清画布：停住那一刻的粒子连线会留在屏幕上，看起来像画面卡住（issue #54）")
	}
	wipe := poolStatsFuncBody(t, src, "bgfWipe")
	if !strings.Contains(wipe, "clearRect(") {
		t.Error("bgfWipe 没有真的清画布：bgfStop / 回到可见时抹掉旧帧都失效（issue #54）")
	}
}

// TestBgfStartIsRepeatable 钉住 #54 引入的重复调用面：bgfStart 现在会被
// 首屏与「离开 reduce」两处调用，所以它必须幂等——否则切几个来回就有两个
// 循环同时在跑，还会给一次 window resize 挂上好几个 bgfResize。
func TestBgfStartIsRepeatable(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "bgfStart")

	if !strings.Contains(body, "if (BGF.raf) return;") {
		t.Error("bgfStart 没有「已在跑就返回」的守卫：重复调用会起第二个 rAF 循环（issue #54）")
	}
	if !strings.Contains(body, "if (!BGF.wired)") {
		t.Error("bgfStart 没有把 resize 监听限成只挂一次：离开 reduce 几个来回后一次 resize 会跑好几遍 bgfResize（issue #54）")
	}
}

// TestSparkChecksThePreferenceAtUseTime 钉住 issue #54 的第二块：火花。
// initSpark 顶上那个守卫只在建监听时查一次；运行中切换之后监听已经挂着，
// 不生成粒子的判断必须在使用点（pointerdown 回调）里再做一次。
func TestSparkChecksThePreferenceAtUseTime(t *testing.T) {
	src := poolStatsUISource(t)

	// 取出 pointerdown 回调体：从 "window.addEventListener(\"pointerdown\"" 到
	// 它的 passive 收尾。用 initSpark 的函数体划范围，避免匹配到别处的监听。
	spark := poolStatsFuncBody(t, src, "initSpark")
	start := strings.Index(spark, `window.addEventListener("pointerdown"`)
	if start < 0 {
		t.Fatal("initSpark 里找不到 pointerdown 监听（issue #54）")
	}
	cb := spark[start:]
	if !strings.Contains(cb, "if (motionReduce()) return;") {
		t.Fatal("pointerdown 回调里没有判 motionReduce()：运行中切到 reduce 之后每点一下照样迸射 9 枚粒子（issue #54）")
	}
	// 判断必须在生成粒子之前——写在 push 之后等于没拦。
	guard := strings.Index(cb, "if (motionReduce()) return;")
	spawn := strings.Index(cb, "SPARK.pts.push(")
	if guard < 0 || spawn < 0 || guard > spawn {
		t.Error("motionReduce() 的判断不在生成粒子之前：粒子照样会被创建（issue #54）")
	}
}
