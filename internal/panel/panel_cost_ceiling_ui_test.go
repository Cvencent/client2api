package panel

import (
	"strings"
	"testing"
)

// 面板常驻性能热点一类（issue #91 + #65）。
//
// 两单同型：面板是「一直开着守一个网关」的界面，而这两处成本是**无条件的**——
// 一层不承载任何信息的装饰画布按屏幕刷新率整块重绘（#91），一批只进不出的
// 铭牌贴图跟着读数每拍长大（#65）。两处都不报错、功能也正常，只是在时间里
// 持续付费，所以判据是「成本有没有上界/有没有条件」，不是「画得对不对」。

// TestBackgroundFieldHasACostCeiling 钉住 #91：粒子场的三处成本约束。
func TestBackgroundFieldHasACostCeiling(t *testing.T) {
	src := poolStatsUISource(t)

	// ① 像素量封顶：dpr 不能无条件取到 2——2x 屏上背板 4.85 Mpx，而粒子数
	// （与 dpr 无关）只从 25 涨到 46，像素翻了四倍买到的只是更细的线。
	if !strings.Contains(src, "const BGF_MAX_PX = 1.3e6;") {
		t.Fatal("粒子场没有像素上限常量 BGF_MAX_PX：2x 屏上整块背板按面积付每帧的钱（issue #91）")
	}
	resize := poolStatsFuncBody(t, src, "bgfResize")
	if !strings.Contains(resize, "BGF_MAX_PX") {
		t.Error("bgfResize 没有按上限压 dpr：像素量仍然随 devicePixelRatio 翻四倍（issue #91）")
	}
	if !strings.Contains(resize, "BGF.dpr = Math.max(0.5, d)") {
		t.Error("bgfResize 没有给 dpr 一个下限：上限在超大视口下会把背板压到几乎看不见（issue #91）")
	}

	// ② 降频：循环必须自己节流，不能按屏幕刷新率跑。
	if !strings.Contains(src, "const FRAME_MS = 1000 / 30;") {
		t.Error("粒子场没有降到 ~30fps：纯氛围层按屏幕刷新率跑是纯浪费（issue #91）")
	}
	start := poolStatsFuncBody(t, src, "bgfStart")
	if !strings.Contains(start, "ts - last < FRAME_MS") {
		t.Error("bgfStart 的循环没有节流判断：FRAME_MS 算了没人用（issue #91）")
	}
	// 节流必须发生在绘制之前——写在重绘之后等于没节流。
	idxGate := strings.Index(start, "ts - last < FRAME_MS")
	idxClear := strings.Index(start, "ctx.clearRect(0, 0, w, h)")
	if idxGate < 0 || idxClear < 0 || idxGate > idxClear {
		t.Error("节流判断不在 clearRect 之前：每帧照样整块重绘（issue #91）")
	}

	// ③ 不可见时不画（按 Page Visibility，不按窗口失焦——面板常驻侧屏是有意在看）。
	if !strings.Contains(start, `document.visibilityState === "hidden"`) {
		t.Error("粒子场在页面不可见时照旧绘制：后台/隐藏标签仍占一帧预算（issue #91）")
	}
	if !strings.Contains(src, `visibilityState === "visible") bgfWipe()`) {
		t.Error("从隐藏回到可见时没有抹掉旧帧：屏幕上会先闪一下冻结的粒子连线（issue #91）")
	}
	// 失焦不停这条是有意的取舍，注释里必须写明，否则下一个人会「顺手补上」。
	if !strings.Contains(src, "窗口失焦") {
		t.Error("注释没有写明「只按 Page Visibility、不按失焦」的取舍：下一个人会顺手改成失焦即停（issue #91）")
	}
}

// TestTextureCachesAreBounded 钉住 #65：三处贴图缓存的键里有会变的读数，
// 缓存必须有上界、淘汰要 dispose、且不能拆掉正在用的那一张。
func TestTextureCachesAreBounded(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, "const TEX_CACHE_MAX = 96;") {
		t.Fatal("贴图缓存没有上界常量 TEX_CACHE_MAX：读数一动就多一张，只进不出（issue #65）")
	}
	sweep := poolStatsFuncBody(t, src, "texCacheSweep")
	// 淘汰必须 dispose：只是 delete 的话纹理仍占着纹理单元。
	if !strings.Contains(sweep, "t.dispose") {
		t.Error("texCacheSweep 淘汰时不 dispose：纹理仍占着显存（issue #65）")
	}
	// 正在用的键必须保住——否则会把此刻正在渲染的贴图拆掉，板卡变黑。
	if !strings.Contains(sweep, "texCacheLive.has(k)") {
		t.Error("texCacheSweep 不跳过 live 键：会 dispose 掉正在渲染的贴图（issue #65）")
	}
	// 三处写入点都要登记 live。
	if n := strings.Count(src, `(B3D.liveKeys || (B3D.liveKeys = [])).push(key)`); n != 2 {
		t.Errorf("b3dTexture / b3dBarLabel 的 live 登记点有 %d 处，期望 2 处（issue #65）", n)
	}
	if !strings.Contains(src, `(ATLAS.liveKeys || (ATLAS.liveKeys = [])).push(key)`) {
		t.Error("atlasLabelTexture 没有登记 live 键：星图铭牌会被自己的清理拆掉（issue #65）")
	}
	// 重建结束时要标记 live 并清理，两处都要。
	if n := strings.Count(src, "texCacheSweep(B3D.texCache)"); n != 1 {
		t.Errorf("3D 舞台的清理点有 %d 处，期望 1 处（issue #65）", n)
	}
	if !strings.Contains(src, "texCacheSweep(ATLAS.texCache)") {
		t.Error("星图没有清理点：它的缓存仍然只进不出（issue #65）")
	}
	// 切主题必须是 dispose 而不是丢引用。
	if strings.Contains(src, "B3D.texCache = {};") || strings.Contains(src, "ATLAS.texCache = {};") {
		t.Error("切主题仍用 `texCache = {}` 丢引用：纹理泄漏，来回切两次就漏一批（issue #65）")
	}
	if !strings.Contains(src, "texCacheReset(B3D.texCache)") || !strings.Contains(src, "texCacheReset(ATLAS.texCache)") {
		t.Error("切主题没有调 texCacheReset：贴图没被真正释放（issue #65）")
	}
}

// TestBoardRemountsAreSignatureGated 钉住 #65 的第三部分：轮询不再无条件
// 重建 3D 场景。同一文件里另一条 tick 早就有签名守卫，这条要同口径。
func TestBoardRemountsAreSignatureGated(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "refreshAll")

	if strings.Contains(body, "mountBoard3d(VIEW);\n  if (loud)") {
		t.Fatal("refreshAll 仍无条件 mountBoard3d(VIEW)：每 5 秒一拍都重建场景与贴图（issue #65）")
	}
	if !strings.Contains(body, "sig !== B3D.sig || B3D.view !== VIEW") {
		t.Error("refreshAll 的 3D 重建没有走签名判断：读数没变也重建（issue #65）")
	}
	// 空态/首建路径不受影响：签名判断只在「读数没变」时跳过重建。
	if !strings.Contains(body, "if (document.body.classList.contains(\"mode-3d\"))") {
		t.Error("refreshAll 丢了「只在 3D 模式下重建」的条件（issue #65）")
	}
}
