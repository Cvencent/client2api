package installer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 模块层写盘失败不许再静默（issue #166）。
//
// #34 修掉了面板四个 store 的「写失败只留 `_ =`」，但模块层还有一族：
// `if err := s.saveState(); err != nil { _ = err }` 这种**空体 if**——最恶劣的一处
// 注释还写着 "it is reported" 而同一行把错误扔掉。写不进去时零日志，重启后
// 按上一份更旧的、更宽松的判决继续跑（冷却阶梯退档、判死复活），操作员唯一的
// 线索是「重启后状态回退」而那时日志里一个字都没有。
//
// 本守卫把「空体 if 吞写盘错误」变成可以在 CI 当场红的形状检查：clients/ 下
// 任何 `if <v> := s.save*(...); <v> != nil {` 的块体不允许只有 `_ = <v>` 一行。
// 正常的处理形态（log / return / 收进 errs）都长得多，不会误伤。

// saveIfHead 匹配一行 save 调用的 if 头；错误变量名在代码里比对（Go 的
// regexp 是 RE2，没有反向引用）。
var saveIfHead = regexp.MustCompile(`(?m)^(\t*)if (\w+) := [^\n]*\.save(State)?\(\); \S+ != nil \{\n`)

// bareSaveDiscard 抓连 if 都省了的形态：`_ = s.save()` / `_ = s.saveState()`。
var bareSaveDiscard = regexp.MustCompile(`(?m)^\t*_ = s\.save(?:State)?\(\)\s*$`)

func TestModuleSaveFailuresAreNotSwallowed(t *testing.T) {
	root := filepath.Join("..", "clients")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")

		// 每个 save-if 头：取 if 开花括号之后的块体，剥掉一层注释行后
		// 若只剩 `_ = <同一变量>`，就是空体吞错。
		for _, loc := range saveIfHead.FindAllStringSubmatchIndex(src, -1) {
			// loc[4]:loc[5] is the error variable; loc[6]:loc[7] the optional
			// "State" of saveState -- the compared name is group 2.
			name := src[loc[4]:loc[5]]
			bodyStart := loc[1]
			bodyEnd := closeBraceIndex(src, bodyStart)
			if bodyEnd < 0 {
				continue
			}
			body := src[bodyStart:bodyEnd]
			if isSwallowBody(body, name) {
				line := strings.Count(src[:loc[0]], "\n") + 1
				t.Errorf("%s:%d: 空体 if 吞掉了写盘错误：写失败必须至少记一行日志或原样返回（issue #166）。块体: %q",
					path, line, compress(body))
			}
		}
		for _, loc := range bareSaveDiscard.FindAllStringIndex(src, -1) {
			line := strings.Count(src[:loc[0]], "\n") + 1
			t.Errorf("%s:%d: `_ = s.save…()` 把写盘错误直接扔掉：写失败必须至少记一行日志或原样返回（issue #166）",
				path, line)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// closeBraceIndex returns the index of the `}` that closes the block opened at
// src[open] (open points just past `{`).  It counts depth over braces on line
// starts only, which is enough for these uniform one-block bodies.
func closeBraceIndex(src string, open int) int {
	depth := 1
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// isSwallowBody reports whether the block body says nothing but `_ = name`
// (comment lines ignored).  Anything that logs, returns, or collects is fine.
func isSwallowBody(body, name string) bool {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		if t == "_ = "+name {
			continue
		}
		return false
	}
	return true
}

// compress folds a snippet to one line for the error message.
func compress(s string) string {
	s = strings.ReplaceAll(s, "\t", "")
	return strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(s, " "))
}
