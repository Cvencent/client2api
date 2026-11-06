package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 面板热路径的客户端侧条件请求（issue #223）与流式分片的增量重绘（#224）。
//
// 两件事同属「轮询/流式的固定开销」一族：#223 是服务端早就发 ETag、客户端
// 从不回放（空闲拍整段重下）；#224 是每个 SSE 分片把全部历史拼成 HTML 再
// 整体赋 innerHTML（240 条消息时每片 30ms）。这里的静态钉锁住接线事实，
// 真浏览器的行为验证在隔离实例上做（panelsmoke 不覆盖数据轮询）。
// ---------------------------------------------------------------------------

// TestAPISupportsConditionalETagReplay 钉住 api() 的 etagKey 通道：调用方给
// 键就自动回放 If-None-Match，304 返回 notModified 并更新存档。没有这个通道，
// 服务端为 #192/#208/#127 写的三个 304 分支永远没人走到。
func TestAPISupportsConditionalETagReplay(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "api")

	if !strings.Contains(body, "opt.etagKey") {
		t.Fatal("api() 没有 etagKey 入口：条件请求只能逐调用点手写（issue #223）")
	}
	if !strings.Contains(body, `h["If-None-Match"]`) {
		t.Error("api() 不回放 If-None-Match：ETag 存了也没人带（issue #223）")
	}
	if !strings.Contains(body, "notModified") {
		t.Error("api() 不回报 304：调用方无法区分「没变」与「空数据」（issue #223）")
	}
	if !strings.Contains(src, "const ETAGS =") {
		t.Fatal("ETAGS 存档不见了：api() 的 etagKey 无处存放（issue #223）")
	}
}

// TestHotPollPathsReplayTheirETags 钉住三条热路径各自接线（逐函数体断言，
// 防止只接一条、漏两条的「部分接线」）。ETAGS 键与调用点一一对应。
func TestHotPollPathsReplayTheirETags(t *testing.T) {
	src := poolStatsUISource(t)

	for _, c := range []struct {
		fn, path, key string
	}{
		{"loadPoolStatus", "/panel/api/pool", "pool"},
		{"renderAlerts", "/panel/api/alerts", "alerts"},
		{"chatLoadThreads", "/panel/api/chat/conversations", "chatlist"},
	} {
		body := poolStatsFuncBody(t, src, c.fn)
		if !strings.Contains(body, `api("`+c.path+`", { etagKey: "`+c.key+`" })`) {
			t.Errorf("%s 没有以 etagKey:%q 调 %s：这条热路径仍是每拍全量 200（issue #223）", c.fn, c.key, c.path)
		}
		if !strings.Contains(body, "notModified") {
			t.Errorf("%s 不处理 304：ETag 回放后沿用不了上一份数据（issue #223）", c.fn)
		}
	}

	// 对照组：refreshModelCatalog 是 #33 的既有范式（MODEL_ETAG 手写），不在
	// 本轮改动面，但它也必须还在——「唯一做对的路径」不许在重构中消失。
	models := poolStatsFuncBody(t, src, "refreshModelCatalog")
	if !strings.Contains(models, "If-None-Match") {
		t.Error("refreshModelCatalog 的条件请求没了：模型目录路径退回全量（issue #33 回归）")
	}
}

// TestStreamFragmentsPatchTheLastBubble 钉住 #224：流式分片必须走
// chatPatchLast（只重写正在生成的那条 .bub），不许直接 chatDraw(true) 整段
// 重建——那个调用的成本随历史长度线性增长。chatPatchLast 失配时的整段
// fallback 保留（保底不画错），settle 的最终绘制也必须还是 chatDraw(false)
// （#76 的读屏语义）。
func TestStreamFragmentsPatchTheLastBubble(t *testing.T) {
	src := poolStatsUISource(t)

	// 分片调用点（正文与推理两处）都不再直接整段重画。
	i := strings.Index(src, "if (d.content) {")
	if i < 0 {
		t.Fatal("正文分片分支不在流循环里：看守失去锚点（issue #224）")
	}
	contentSeg := src[i : i+320]
	if strings.Contains(contentSeg, "chatDraw(true)") {
		t.Error("正文分片仍直接 chatDraw(true)：每片整段重建全部历史（issue #224）")
	}
	if !strings.Contains(contentSeg, "chatPatchLast()") {
		t.Error("正文分片没有走 chatPatchLast：增量重绘没接上（issue #224）")
	}

	// chatPatchLast 必须存在且带失配 fallback（不能为了性能把正确性丢了）。
	patch := poolStatsFuncBody(t, src, "chatPatchLast")
	if strings.Count(patch, "chatDraw(true)") < 1 {
		t.Error("chatPatchLast 没有失配 fallback：DOM 对不上时会静默画错（issue #224）")
	}
	if !strings.Contains(patch, "querySelectorAll") {
		t.Error("chatPatchLast 不核对气泡数：失配检测失去依据（issue #224）")
	}

	// settle 的最终绘制保持 chatDraw(false)：完成回复一次宣告（issue #76）。
	if !strings.Contains(src, "chatDraw(false)") {
		t.Fatal("settle 的最终绘制不见了：aria-live 永远停在 off，完成的回复不再播报（issue #76）")
	}
}
