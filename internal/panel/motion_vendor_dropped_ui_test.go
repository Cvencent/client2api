package panel

import (
	"strings"
	"testing"
)

// 零消费者第三方库的移除回归（issue #94）。
//
// motion.min.js 全站零消费（弹簧动效走自研 springValue），却无条件打进二进制
// 并随每次页面加载下载——屏蔽对照实验证明面板功能与动效均不依赖它。移除后，
// 引用链上的每一环都不许回来。

func TestMotionVendorIsGone(t *testing.T) {
	src := poolStatsUISource(t)

	if strings.Contains(src, "motion.min.js") {
		t.Fatal("index.html 仍引用 motion.min.js：零消费的 64 KB 库会随每次加载下载（issue #94）")
	}
	// 误导性注释不许回来：它会让后来人以为动效由 Motion 驱动而不敢清理。
	if strings.Contains(src, "Motion 给全站的弹簧进场编排") {
		t.Fatal("「Motion 给全站的弹簧进场编排」的误导注释回来了（issue #94）：弹簧走自研 springValue")
	}
}
