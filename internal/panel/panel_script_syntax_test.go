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
	src := poolStatsUISource(t)
	blocks := panelScriptBlocks.FindAllStringSubmatch(src, -1)
	if len(blocks) == 0 {
		t.Fatal("index.html has no inline <script> block to check")
	}

	dir := t.TempDir()
	checked := 0
	for i, m := range blocks {
		body := m[1]
		if strings.TrimSpace(body) == "" {
			continue
		}
		checked++
		path := filepath.Join(dir, fmt.Sprintf("block-%d.js", i))
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing inline <script> block %d: %v", i, err)
		}
		if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
			t.Errorf("inline <script> block %d does not parse: %v\n%s", i, err, out)
		}
	}
	if checked == 0 {
		t.Fatal("index.html has no non-empty inline <script> block")
	}
}
