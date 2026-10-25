package panel

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestChatFunctionsAreAllDefined 是一类真实故障的守卫：早前的一次修复把
// chatCurrentThread() 整个删掉了，但 settle() 还在调用它。浏览器直到用户真正
// 发消息、请求失败走进 settle() 才炸出 ReferenceError——编译期、启动期和
// 静态 id 检查全都看不见。
//
// 这里把聊天区块里的 chat* 函数定义和调用各扫一遍，任何「调用了但没有定义」
// 都直接失败。名字全部以 chat 开头，扫描范围也只限这一块，不会误伤全局 helper。
func TestChatFunctionsAreAllDefined(t *testing.T) {
	src := poolStatsUISource(t)
	start := strings.Index(src, "/* ── 对话测试")
	end := strings.Index(src, "const USAGE_SUBTABS")
	if start < 0 || end <= start {
		t.Fatal("找不到对话测试的脚本区块")
	}
	block := src[start:end]

	defRe := regexp.MustCompile(`(?m)(?:async\s+)?function\s+([A-Za-z_$][\w$]*)\s*\(`)
	callRe := regexp.MustCompile(`(?m)(?:^|[^\w.$])(chat[A-Z][A-Za-z0-9_]*)\s*\(`)

	defined := map[string]bool{}
	for _, m := range defRe.FindAllStringSubmatch(block, -1) {
		defined[m[1]] = true
	}
	missing := map[string]bool{}
	for _, m := range callRe.FindAllStringSubmatch(block, -1) {
		if !defined[m[1]] {
			missing[m[1]] = true
		}
	}
	if len(missing) == 0 {
		return
	}
	names := make([]string, 0, len(missing))
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Fatalf("聊天脚本调用了未定义的函数：%s（浏览器只会在运行时抛 ReferenceError）", strings.Join(names, ", "))
}
