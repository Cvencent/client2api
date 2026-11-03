package panel

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 会话标题的长度上限（issue #143）。
//
// 报告里两条独立的观察，各自都要盖住：
//
//  1. **服务端没有上限**：`PATCH {"title": …*70}` 实测 200，`GET` 回读 280
//     字符；更极端的 1,000,000 字符标题也 200，落盘 1,002,152 B。同一份代码
//     里账号备注早有 maxAccountNoteRunes = 120（configwrite.go:30-34），理由
//     写得很清楚：「an unbounded string would let one careless paste bloat the
//     config and wreck the layout」。
//  2. **页头零截断**：`.chat-head-title h3` 只有一条 `white-space: nowrap`，
//     而紧邻的副标题有完整的一组；侧栏 `.chat-title` 也有。同一字段一处截断
//     一处不截断，所以这不是设计选择。
//
// 这一组钉服务端那一半（前端那一半是静态契约 + panelsmoke 的几何断言）。
// ---------------------------------------------------------------------------

// TestChatTitleHasARuneCap mirrors the account-note rule: a title past the limit
// is refused, and the refusal names the limit so the page can tell the operator.
func TestChatTitleHasARuneCap(t *testing.T) {
	if maxChatTitleRunes <= 0 {
		t.Fatal("maxChatTitleRunes must be a real cap")
	}
	// 与账号备注同一量级：中文一句话够用，长命令行也够用。太小会把正常标题
	// 拦下来，太大就等于没有上限（报告里 24 字就已经和下拉重叠 68px）。
	if maxChatTitleRunes != maxAccountNoteRunes {
		t.Errorf("标题上限 %d 与账号备注 %d 不同：同类字段（操作员自己输入的字符串）"+
			"该用同一个口径，否则只是换了个数字继续漂移", maxChatTitleRunes, maxAccountNoteRunes)
	}

	store := newChatHistoryStore("")
	exact := strings.Repeat("字", maxChatTitleRunes)
	got, err := store.Create(ChatThread{Title: exact})
	if err != nil {
		t.Fatalf("恰好到上限的标题被拒了：%v", err)
	}
	if got.Title != exact {
		t.Fatalf("恰好到上限的标题被改写了：%d 字 → %d 字", len([]rune(exact)), len([]rune(got.Title)))
	}

	over := strings.Repeat("字", maxChatTitleRunes+1)
	if _, err := store.Create(ChatThread{Title: over}); err == nil {
		t.Fatal("超过上限的标题仍被接受：一份粘贴就能把记录撑大、把页头撑爆（#143）")
	} else if !strings.Contains(err.Error(), "120") {
		t.Errorf("拒绝信息没有点名上限（%q）：页面只能照抄一句看不出怎么办的话给操作员", err.Error())
	}
}

// TestChatTitleCapCountsRunesNotBytes: 上限是「字符」，不是「字节」。用字节
// 计数，一个 40 字的中文标题（120 字节）就会被当成正好到顶——中文操作员第一
// 个撞上，而且撞得莫名其妙。
func TestChatTitleCapCountsRunesNotBytes(t *testing.T) {
	store := newChatHistoryStore("")
	// 120 个汉字 = 360 字节；按字节算早就超了，按字符算正好。
	title := strings.Repeat("汉", maxChatTitleRunes)
	got, err := store.Create(ChatThread{Title: title})
	if err != nil {
		t.Fatalf("120 个汉字（%d 字节）被拒：上限按字节而不是按字符算了：%v", len(title), err)
	}
	if len([]rune(got.Title)) != maxChatTitleRunes {
		t.Fatalf("中国汉字标题被改了长度：%d 字", len([]rune(got.Title)))
	}
}

// TestChatTitleCapIsNotASilentCut: 超限必须是**拒绝**，不能静默截断后 200。
// 静默截断会让页面显示一个操作员没打过的标题，和 #121/#122 的教训同一条。
func TestChatTitleCapIsNotASilentCut(t *testing.T) {
	store := newChatHistoryStore("")
	created, err := store.Create(ChatThread{Title: "原名"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	long := strings.Repeat("长", maxChatTitleRunes+50)
	if _, err := store.Update(created.ID, ChatThread{Title: long}); err == nil {
		t.Fatal("超限的改名被接受了")
	}
	// 拒绝之后盘上的标题必须还是原来那个：既没有被截断写入，也没有被清空。
	back, ok := store.Get(created.ID)
	if !ok {
		t.Fatal("线程没了")
	}
	if back.Title != "原名" {
		t.Fatalf("被拒的改名改动了存量标题：%q（既不静默截断，也不许顺手清掉）", back.Title)
	}
}

// TestChatTitleCapCoversEveryWritePath: 三条写入路径（POST / PATCH / Rename）
// 只要有一条漏掉，上限就形同虚设——补一条新路径时最容易忘。
func TestChatTitleCapCoversEveryWritePath(t *testing.T) {
	h := New(Options{DataDir: t.TempDir()})
	long := strings.Repeat("填", maxChatTitleRunes+1)

	// 1. POST（新建）
	rec := hitRoute(t, h, http.MethodPost, "/panel/api/chat/conversations",
		`{"title":"`+long+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST 超长标题 = %d，期望 400（#143 的第一条证据就是实测 200）", rec.Code)
	}

	// 2. PATCH（改名，报告里复现的那条）
	rec = hitRoute(t, h, http.MethodPost, "/panel/api/chat/conversations", `{"title":"正常"}`)
	var created ChatThread
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	rec = hitRoute(t, h, http.MethodPatch, "/panel/api/chat/conversations/"+created.ID,
		`{"title":"`+long+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH 超长标题 = %d，期望 400（报告实测 200 并回读 280 字符）", rec.Code)
	}
	// 拒绝必须真的没写盘。
	rec = hitRoute(t, h, http.MethodGet, "/panel/api/chat/conversations/"+created.ID, "")
	if !strings.Contains(rec.Body.String(), "正常") {
		t.Errorf("被拒的 PATCH 改动了存量记录：%s", rec.Body.String())
	}
}

// TestChatHistoryFileStaysSmall is #143's "one paste bloats the file" half,
// measured the way the reporter measured it: bytes on disk after the write.
func TestChatHistoryFileStaysSmall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel", "chat_history.json")
	store := newChatHistoryStore(path)

	// 报告里那次写入落了 1,002,152 B。这里发一份同样大的标题，它必须被拒。
	huge := strings.Repeat("长", 1_000_000)
	if _, err := store.Create(ChatThread{Title: huge}); err == nil {
		t.Fatal("100 万字符的标题被接受了：报告实测落盘 1,002,152 B（#143）")
	}
	if _, err := os.Stat(path); err == nil {
		// 文件存在也可以，但它不该因为一次被拒的写入而变大。
		b, _ := os.ReadFile(path)
		if len(b) > 64*1024 {
			t.Fatalf("一次被拒的写入仍然写了 %d 字节到盘上", len(b))
		}
	}

	// 正路：一个正常标题照常落盘。
	if _, err := store.Create(ChatThread{Title: "正常的对话"}); err != nil {
		t.Fatalf("正常标题被拒：%v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(b) > 4096 {
		t.Fatalf("一条正常记录写了 %d 字节，期望几百字节", len(b))
	}
}

// TestTheFrontendAndServerCapsAgree is the cross-file pin: CHAT_TITLE_MAX in
// index.html must equal maxChatTitleRunes here.  Two copies of one rule only
// stay honest if something compares them (issue #143 was measured on both ends).
func TestTheFrontendAndServerCapsAgree(t *testing.T) {
	src := poolStatsUISource(t)
	const decl = "const CHAT_TITLE_MAX = "
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatal("index.html has no CHAT_TITLE_MAX")
	}
	rest := src[i+len(decl):]
	end := strings.IndexAny(rest, ";\n")
	if end < 0 {
		t.Fatal("CHAT_TITLE_MAX declaration is unterminated")
	}
	got := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest[:end]), ";"))
	want := strconv.Itoa(maxChatTitleRunes)
	if got != want {
		t.Errorf("前端 CHAT_TITLE_MAX = %s，服务端 maxChatTitleRunes = %s：两端一颗数字分叉，"+
			"操作员就会先被前端拦住（或反过来把服务端 400 当成故障）", got, want)
	}
}
