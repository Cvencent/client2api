package raccoon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 自动登录的浏览器 profile 必须跨尝试复用（issue #141，方向 1 的定性结论）。
//
// 操作员的对照结论（2026-11-03 13:08，issue #141 评论区原话「他没有显式验证，
// 你要用真实浏览器就行」）：官方客户端连滑块都不弹——所以拒绝面是**自动化
// 指纹被识别**，不是 IP/设备信誉。方向 1 到此定性，剩下的是代码面能改的部分。
//
// 而自动化侧最像「真浏览器」的差距在这里：Launch() 默认为每次尝试
// `os.MkdirTemp` 一个**全新** profile，Close 时整个删掉——对厂商的设备画像
// 来说，每次都是一台没有任何历史的全新机器。官方客户端能过，正是因为它带着
// 积累的状态。修法：profile 固定在模块数据目录下、跨尝试复用，让分数像真
// 浏览器那样积累（第一次仍可能被拒——分数要从某处开始涨——但不再是每次运行
// 都从零开始）。
//
// 判据因此不是「有 profile 目录」，而是：
//   - profile 根**必须**来自模块数据目录（一个固定位置，而不是 temp）；
//   - 现有的有头守卫继续生效（headless 是 #63 的死因，两条修法互相独立）。
// ---------------------------------------------------------------------------

func TestAutoLoginProfileIsPersistentNotThrowaway(t *testing.T) {
	raw, err := os.ReadFile("autologin.go")
	if err != nil {
		t.Fatalf("读不到 autologin.go：%v", err)
	}
	src := string(raw)

	// Launch 必须带 Profile（#183：真正的持久 user-data-dir，Close 不删），
	// 且指向固定位置。曾用 ProfileRoot——那只是「固定了一次性 profile 的
	// 父目录」，MkdirTemp+删除原样在跑，login-browser 目录始终为空，#141 的
	// 「积累历史」一天都没发生过。
	if !strings.Contains(src, "Profile:") {
		t.Fatal("自动登录的 Launch 没有 Profile：每次尝试都是一个全新的一次性 profile，" +
			"对厂商设备画像来说是台没有历史的机器（issue #141/#183：官方客户端带着积累状态能过，" +
			"自动化两次被拒）")
	}
	if strings.Contains(src, "ProfileRoot:") {
		t.Error("还在用 ProfileRoot：它只固定一次性 profile 的父目录，不是持久 profile（issue #183）")
	}
	// profile 根必须从数据目录派生（固定位置），而不是另起 temp。
	if !strings.Contains(src, "raccoonLoginProfileRoot(") {
		t.Error("没有 raccoonLoginProfileRoot：profile 位置没有固定的派生来源")
	}

	// 派生函数本身：数据目录在 → 数据目录下的固定名；不在 → 工作目录下的同位置。
	if got := raccoonLoginProfileRoot("D:/data"); filepath.ToSlash(got) != "D:/data/login-browser" {
		t.Errorf("raccoonLoginProfileRoot(\"D:/data\") = %q, want D:/data/login-browser", got)
	}
	if got := raccoonLoginProfileRoot(""); strings.Contains(filepath.ToSlash(got), "Temp") || strings.Contains(filepath.ToSlash(got), "temp") {
		t.Errorf("空数据目录的 profile 根 %q 落在临时目录：那正是「一次性」要避免的形状", got)
	}

	// 生产路径必须把数据目录接进去：StartAutoLogin 的回退 driver 要带上根。
	if !strings.Contains(src, "raccoonBrowserDriver{profileRoot: raccoonLoginProfileRoot(c.deps.DataDir)}") {
		t.Error("StartAutoLogin 的回退 driver 没有接数据目录：生产路径仍是默认的一次性行为")
	}

	// 反向钉子：不能为了「隔离」退回 per-attempt 的 MkdirTemp（browser 包自己的
	// 默认行为就是它，这里出现即说明绕回了）。
	launch := src[strings.Index(src, "browser.Launch(ctx"):]
	if i := strings.Index(launch, "\n}\n"); i >= 0 {
		launch = launch[:i]
	}
	if strings.Contains(launch, "MkdirTemp") {
		t.Error("Launch 附近又出现了 MkdirTemp：profile 又变成每次新建（issue #141）")
	}
}
