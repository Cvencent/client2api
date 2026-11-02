package panel

import (
	"strings"
	"testing"
)

// 面板 UX 状态/提示不同步一类的回归：
//   - #105 任务中心「成长任务队列」空态与 chips 自相矛盾滞留
//   - #106「/」快捷键只接了 3 个视图，另外 6 个占位写着「（/）」的搜索框按了没反应
//
// 两单同型：同屏两组信息各画各的、互不同步（#61/#41 家族）。

// TestRenderQCResetsTheStaleEmptyState 钉住 #105：客户端存在时，上一轮空分支
// 写进 #tcEmpty 的「还没有客户端提供任务看板」必须归位（恢复静态默认并藏起），
// 否则它与同时画出的 chips 自相矛盾滞留到用户真的点一次扫描。
func TestRenderQCResetsTheStaleEmptyState(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderQC")

	if !strings.Contains(body, `if (!cur) {`) {
		t.Fatal("renderQC 的空分支结构变了；#105 的归位必须挂在「客户端存在」那侧")
	}
	// 归位动作三件套：恢复静态默认标题、恢复静态默认说明、藏起空态。
	for _, want := range []string{
		`empty.querySelector(".t").textContent = "还没有扫描过"`,
		`empty.style.display = "none"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderQC 缺少空态归位语句 %q：假文案会与 chips 同屏滞留（issue #105）", want)
		}
	}
	// 归位必须发生在 !cur 分支之后（即客户端存在的那条路上）。
	guard := strings.Index(body, "if (!cur) {")
	reset := strings.Index(body, `empty.querySelector(".t").textContent = "还没有扫描过"`)
	if guard < 0 || reset < 0 || reset < guard {
		t.Fatal("空态归位不在 !cur 分支之后：客户端存在时不会恢复（issue #105）")
	}
}

// TestSlashShortcutResolvesTheVisibleSearchBox 钉住 #106：目标不再按视图 id
// 写死，而是当前视图内第一个可见的 search/.filt 输入框；对话页不再把焦点挪去
// 高级面板的「缩小模型列表」，而是主消息框。
func TestSlashShortcutResolvesTheVisibleSearchBox(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `document.getElementById("view-" + VIEW)`) {
		t.Fatal("「/」快捷键没有按当前视图解析目标：新增子页的搜索框仍会没反应（issue #106）")
	}
	if !strings.Contains(src, `input[type=search], input.filt`) {
		t.Fatal("「/」快捷键没有查找 search/.filt 输入框（issue #106）")
	}
	if !strings.Contains(src, "r.width > 0 && r.height > 0") {
		t.Fatal("「/」快捷键没有按可见性过滤（隐藏子面板里的搜索框会被选中，issue #106）")
	}
	// 对话页落点：主消息框，不是被强制展开的高级面板。
	if !strings.Contains(src, `VIEW === "chat" ? $("#chatInput") : null`) {
		t.Fatal("对话页的「/」没有落到主输入框：旧行为会把焦点挪去「缩小模型列表」（issue #106）")
	}
	if strings.Contains(src, `VIEW === "chat" ? chatOpenAdvanced(true) : null`) {
		t.Error("对话页的「/」退回强制展开高级面板的旧行为（issue #106 回归）")
	}
}
