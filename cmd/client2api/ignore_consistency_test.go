package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 「规则说排除、库里却有」的仓库状态不一致必须当场红（issue #167）。
//
// output/playwright/ 下两个 Playwright 探针在 0.1.31 就进了库，五天后 .gitignore
// 才加上 `/output/playwright/*.js` —— 加规则的人想清掉这一类文件，但漏了
// `git rm --cached`：tracked 文件不因 ignore 而消失。此后 `git status --ignored`
// 在该目录什么都不报，而 `git add -A` 照样把它们的修改提交进公开仓库——这类
// 探针脚本天然带本机盘符路径与回环端口，正是「本机事实不进公开仓库」纪律的口子。
//
// 既有 packaging 门禁只断言 .gitignore 的**文本**里有某条规则；本守卫断言的是
// **仓库状态**：没有任何 tracked 文件违着 ignore 规则。`git ls-files -i -c
// --exclude-standard` 列出的就是这种文件，必须为空。
func TestNoTrackedFileViolatesTheIgnoreRules(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH: the guard runs where the repo is developed and CI")
	}
	// go test 的进程目录是包目录（cmd/client2api），git ls-files 在子目录里
	// 只会列出相对它的结果——红检实测会在「明明有违例」时空手而归。-C 指到
	// 仓库根（本测试文件在其下两层）才量的是整仓。
	root := filepath.Join("..", "..")
	cmd := exec.Command(git, "-C", root, "ls-files", "-i", "-c", "--exclude-standard")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files -i -c --exclude-standard: %v", err)
	}
	violations := strings.TrimSpace(string(out))
	if violations != "" {
		t.Errorf("这些文件被 .gitignore 排除、却仍在版本库里（加规则时漏了 git rm --cached，issue #167）：\n%s", violations)
	}
}
