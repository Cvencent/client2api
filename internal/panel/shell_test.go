package panel

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The shell is one embedded file with no build step, so nothing in the package
// reads the markup: a renamed element and a stale $("#old-name") are both
// invisible until somebody opens the page and the button does nothing.  This is
// the only automated check that the script and the markup agree on the ids, and
// it is what makes renaming an element safe to do at all.
// ---------------------------------------------------------------------------

var (
	// shellIDAttr is an id written out in the markup -- or in one of the HTML
	// strings the script injects, which is where most of the tables come from.
	shellIDAttr = regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`)
	// shellMinted is an id built at runtime: the first argument of a row helper
	// becomes id="..." (see each body), so the literal never appears next to
	// the attribute and the rule above cannot see it.  Every helper that mints
	// an element must be listed here -- a new one that is not would make its
	// rows look dangling, which is the guard doing its job.
	shellMinted = regexp.MustCompile(`(?:cfgText|cfgNum|cfgCheck|pfWBText|pfWBNum|pfWBCheck)\("([A-Za-z0-9_-]+)"`)
	// A manual-add field is minted as "mf_" + its AccountFields key, so the
	// id is data (mf_provider ...) rather than a literal in the markup.  The
	// script may look one up for the fields it knows by key.
	shellDynamicID = regexp.MustCompile(`"mf_"\s*\+\s*f\.key`)
)

// shellRefs are the ways the script looks an element up.  Each captures only an
// exact leading #id, so a selector with a descendant part still yields the id
// that has to exist.  Lookups built from a computed string cannot be checked
// statically and are deliberately not matched.
var shellRefs = []*regexp.Regexp{
	regexp.MustCompile(`\$\("#([A-Za-z0-9_-]+)"\)`),
	regexp.MustCompile(`getElementById\("([A-Za-z0-9_-]+)"\)`),
	regexp.MustCompile(`querySelector(?:All)?\("#([A-Za-z0-9_-]+)["' ]`),
}

// shellDanglingIDs returns, sorted and de-duplicated, every id the script looks
// up but that no id="..." in the file ever declares.
func shellDanglingIDs(src string) []string {
	// Only the two minting patterns define anything.  Adding shellRefs here
	// would be circular -- every id the script looks up would count as defined
	// by virtue of being looked up, and the guard could never fail.
	defined := map[string]bool{}
	for _, re := range []*regexp.Regexp{shellIDAttr, shellMinted} {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			defined[m[1]] = true
		}
	}
	seen := map[string]bool{}
	dangling := []string{}
	for _, re := range shellRefs {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			id := m[1]
			if defined[id] || seen[id] {
				continue
			}
			seen[id] = true
			dangling = append(dangling, id)
		}
	}
	sort.Strings(dangling)
	return dangling
}

// shellDuplicateIDs returns, sorted, every id="..." literal the file declares
// more than once.  It scans the whole file, so ids the script injects through
// innerHTML template strings count too -- those are exactly the ones a human
// eye skips.
func shellDuplicateIDs(src string) []string {
	counts := map[string]int{}
	for _, m := range shellIDAttr.FindAllStringSubmatch(src, -1) {
		counts[m[1]]++
	}
	dups := []string{}
	for id, n := range counts {
		if n > 1 {
			dups = append(dups, fmt.Sprintf("%s(x%d)", id, n))
		}
	}
	sort.Strings(dups)
	return dups
}

// TestShellHasNoDuplicateElementIDs 既然已经开始扫 id，就把重复也一并守住。
// getElementById 和 $("#x") 只认第一个同名元素，重复的 id 会让后一个永远拿不到
// 事件。这不是假设：一键连接的卡片就曾经被渲染进任务中心同名的 qcList，点
// 「检测服务」完全没反应。
func TestShellHasNoDuplicateElementIDs(t *testing.T) {
	src := string(indexHTML)
	if dups := shellDuplicateIDs(src); len(dups) > 0 {
		t.Errorf("shell 里有重复的 id（同名元素只有第一个能被 $ 拿到）：%s", strings.Join(dups, ", "))
	}
}

// TestShellDuplicateIDGuardFlagsARepeat 是上面那个守卫的反向证据：一个被渲染
// 两次的模板必须被抓出来，否则守卫在真出事时是不会叫的。
func TestShellDuplicateIDGuardFlagsARepeat(t *testing.T) {
	const src = `<div id="once"></div>
<div id="twice"></div>
<script>const t = '&lt;span id="twice"&gt;'</script>`
	got := strings.Join(shellDuplicateIDs(src), ",")
	if want := "twice(x2)"; got != want {
		t.Fatalf("duplicate ids = %q, want %q", got, want)
	}
}

func TestShellScriptOnlyUsesIDsTheMarkupDefines(t *testing.T) {
	src := string(indexHTML)
	if strings.TrimSpace(src) == "" {
		t.Fatal("index.html was not embedded")
	}
	if dangling := shellDanglingIDs(src); len(dangling) > 0 {
		t.Errorf("the script looks up id(s) that appear nowhere in the shell: %s", strings.Join(dangling, ", "))
	}
}

// TestShellIDGuardFlagsAMissingElement is the negative control for the guard
// above: without it the guard could be circular (every referenced id counting as
// its own definition) and pass on a shell that is falling apart -- which is
// exactly the bug it is here to catch, so it gets its own evidence.
func TestShellIDGuardFlagsAMissingElement(t *testing.T) {
	const src = `<div id="real"></div>
<script>
  const a = $("#real");
  const b = $("#renamed");
  const c = document.getElementById("gone");
  const d = document.querySelector("#also-gone .row");
  const e = $("#real").value;
</script>`
	got := strings.Join(shellDanglingIDs(src), ",")
	if want := "also-gone,gone,renamed"; got != want {
		t.Fatalf("dangling ids = %q, want %q", got, want)
	}
}

// TestShellEndsAtTheClosingHTMLTag 钉住 </html> 之后没有多余文本：HTML 解析器会
// 把结束标签之后的字符重新塞回 <body>，于是那截 CSS 注释残片会变成页面底部
// 的可见文字，还多出一条本不该有的竖向滚动条。
func TestShellEndsAtTheClosingHTMLTag(t *testing.T) {
	src := string(indexHTML)
	end := strings.LastIndex(src, "</html>")
	if end < 0 {
		t.Fatal("index.html 没有 </html>：嵌入的资源不是一份完整文档")
	}
	tail := strings.TrimSpace(src[end+len("</html>"):])
	if tail == "" {
		return
	}
	if len(tail) > 80 {
		tail = tail[:80] + "..."
	}
	t.Errorf("</html> 之后还有内容（会被解析回 <body> 变成可见文字）：%q", tail)
}

// shellRule 返回第一条以 prefix 开头的 CSS 规则（含前后的注释不算），到右括号为止。
func shellRule(src, prefix string) string {
	i := strings.Index(src, prefix)
	if i < 0 {
		return ""
	}
	j := strings.Index(src[i:], "}")
	if j < 0 {
		return ""
	}
	return src[i : i+j+1]
}

// TestSavebarSticksToTheViewport 钉住「保存」按钮永远看得见。
//
// 保存栏自己写着 position:sticky;bottom:0，但只要祖先 .box 是 overflow:hidden，
// 那个盒子就成了最近的可滚动祖先，而它根本不滚 —— sticky 退化成普通文档流。
// 配置页的表单有四千多像素高，保存按钮被顶到页面最下面，操作员只会上来问
// 「怎么没有保存按钮」。这两个声明必须成对地留着，改一个就要改另一个。
func TestSavebarSticksToTheViewport(t *testing.T) {
	src := string(indexHTML)

	box := shellRule(src, ".box {")
	if box == "" {
		t.Fatal("找不到 .box 规则：面板块的裁剪方式决定了粘底能否生效")
	}
	if strings.Contains(box, "overflow: hidden") {
		t.Errorf(".box 又用回了 overflow:hidden，它会让 .savebar 的粘底失效：%s", box)
	}
	if !strings.Contains(box, "overflow: clip") {
		t.Errorf(".box 不再用 overflow:clip：圆角会漏内容，或者粘底会再度失效：%s", box)
	}

	bar := shellRule(src, ".savebar {")
	if bar == "" {
		t.Fatal("找不到 .savebar 规则")
	}
	for _, want := range []string{"position: sticky", "bottom: 0"} {
		if !strings.Contains(bar, want) {
			t.Errorf(".savebar 丢了粘底声明 %q：%s", want, bar)
		}
	}
}

// TestShellAnnouncesBootCompletion 钉住 cmd/panelsmoke 依赖的启动契约。
//
// 打包脚本和安装器都用无头浏览器打开 /panel/，然后找
// <html data-c2a-ready="1">。那段契约就在下面：boot() 必须在 refreshAll()
// 成功之后才打标记，并把启动抛错、未捕获异常、未处理的 Promise 拒绝都变成
// data-c2a-boot-error，这样 0.1.5 那种「服务端 200、页面全死」在打包阶段
// 就会失败，而不是等用户装完才发现。
func TestShellAnnouncesBootCompletion(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`document.documentElement.dataset.c2aReady = "1"`,
		`document.documentElement.dataset.c2aBootError = "1"`,
		`window.addEventListener("error"`,
		`window.addEventListener("unhandledrejection"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("面板不再声明启动契约 %q；cmd/panelsmoke 和安装器自检都依赖它", want)
		}
	}

	// The ready marker has to come after the first data load: a panel that
	// throws while rendering its first view must not look healthy.
	refresh := strings.Index(src, "await refreshAll();")
	if refresh < 0 {
		t.Fatal("boot() 不再等待第一次 refreshAll()")
	}
	if ready := strings.Index(src, `document.documentElement.dataset.c2aReady = "1"`); ready < refresh {
		t.Error("c2aReady 标记必须在第一次 refreshAll() 之后才设置")
	}
}
