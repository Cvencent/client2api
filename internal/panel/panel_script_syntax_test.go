package panel

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 整个内联面板脚本的语法回归。
//
// 面板的 JavaScript 全是字符串拼接，Go 编译器完全不看它；而面板的静态 UI
// 测试只断言「某段文字在不在」——一个多余的 }（比如把对象字面量提前闭合）
// 能让每一条字面断言照旧通过，同时整个脚本在浏览器里一句都不执行：页面永远
// 停在 data-c2a-ready 之前，每个按钮都是死的。
//
// 0.1.27 就是这样翻车的：SC_BATCH_LABELS 对象在 recovery 之后被提前 }; 关掉，
// daily_balance 掉到对象外面，直到打包时的 panelsmoke 才暴露。这里用 node 把
// <script> 抽出来做 --check，让同类问题在 go test 阶段就红，而不是等到打安装包。
//
// node 只是可选依赖：没有就跳过。面板本身不需要 node。
// ---------------------------------------------------------------------------

// panelScriptBlocks 抓出每一段内联 <script>…</script> 的内容。带 src 的外链
// 脚本抓到空串，会被下面的 TrimSpace 跳过。
var panelScriptBlocks = regexp.MustCompile(`(?s)<script\b[^>]*>(.*?)</script>`)

func TestPanelInlineScriptsAreSyntacticallyValid(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the inline script syntax check needs it")
	}

	dir := t.TempDir()
	total := 0
	// Every embedded document, not one hardcoded name (issue #128).
	for _, doc := range embeddedHTMLDocs {
		b, err := os.ReadFile(doc)
		if err != nil {
			// Not a skip: a listed document that cannot be read is the bug.
			t.Errorf("reading %s: %v", doc, err)
			continue
		}
		blocks := panelScriptBlocks.FindAllStringSubmatch(string(b), -1)
		if len(blocks) == 0 {
			t.Errorf("%s has no inline <script> block: either the document lost its script or the "+
				"extraction regex no longer matches it (issue #128)", doc)
			continue
		}
		checked := 0
		for i, m := range blocks {
			body := m[1]
			if strings.TrimSpace(body) == "" {
				continue
			}
			checked++
			total++
			path := filepath.Join(dir, fmt.Sprintf("%s-block-%d.js", doc, i))
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("writing inline <script> block %d of %s: %v", i, doc, err)
			}
			if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
				t.Errorf("inline <script> block %d of %s does not parse: %v\n%s", i, doc, err, out)
			}
		}
		if checked == 0 {
			t.Errorf("%s has no non-empty inline <script> block (issue #128)", doc)
		}
	}
	if total == 0 {
		t.Fatal("no inline <script> block was checked across any embedded document")
	}
}

// embeddedHTMLDocs are the HTML documents this package go:embed's and the
// browser executes.  There are TWO (issue #128):
//
//	index.html    the panel shell, served at /panel/
//	captcha.html  the vendor captcha page, loaded in a same-origin iframe by
//	              runCaptcha() -- a separate document with its own script
//
// The guard above used to read a single hardcoded filename, so the second
// document sat outside every automated check: the reporter broke captcha.html's
// script (one stray brace, the same shape as the 0.1.27 shell breakage) and all
// five gates stayed green, while the page died with "Unexpected token" and the
// captcha overlay never closed.
var embeddedHTMLDocs = []string{"index.html", "captcha.html"}

// minEmbeddedHTMLDocs is a floor, not a wish: dropping a name from the list
// would silently shrink the guard back to covering less, which is how #128
// happened.  Raise it deliberately when a document is added.
const minEmbeddedHTMLDocs = 2

func TestTheGateCoversEveryEmbeddedHTMLDocument(t *testing.T) {
	if len(embeddedHTMLDocs) < minEmbeddedHTMLDocs {
		t.Fatalf("embeddedHTMLDocs lists %d document(s), want at least %d: a dropped name takes that "+
			"document out of every syntax gate (issue #128)", len(embeddedHTMLDocs), minEmbeddedHTMLDocs)
	}
	for _, name := range embeddedHTMLDocs {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("embeddedHTMLDocs names %s, which does not exist beside this test: %v", name, err)
		}
		// The list must match what is embedded, or it is a comment: both
		// directives live in this package, so reading them is enough.
		found := false
		for _, src := range []string{"panel.go", "captcha.go"} {
			b, err := os.ReadFile(src)
			if err != nil {
				continue
			}
			if strings.Contains(string(b), "//go:embed "+name) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is listed as embedded but no //go:embed directive names it: the guard would "+
				"check a document nothing serves (issue #128)", name)
		}
	}
}
