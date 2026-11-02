package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 干净 clone 上安装器契约测试的可重复性（issue #71）。
//
// payload/ 是 build.ps1 的构建产物，gitignore 只留 .gitkeep：一个干净的
// checkout 里 embed FS 是空的，对着它断言「写出的文件」的三个测试曾硬失败——
// 新人按 AGENTS.md 第一次跑 go test ./... 就是红的，红在与他的改动无关的
// 安装器上。修复分两半，这里钉住测试侧：三个测试在 payload 缺席时 skip
// （照 TestSmokeTestPanelHashFallback 的先例），让失败只落在「真的坏了」上；
// Makefile 侧的 `make payload` 目标是另一半。

func TestPayloadAbsentContractTestsSkip(t *testing.T) {
	b, err := os.ReadFile("install_test.go")
	if err != nil {
		t.Skipf("cannot read install_test.go: %v", err)
	}
	src := string(b)

	// 三个契约测试都必须有 payload 缺席的 skip 先导。
	const skipLine = `t.Skip("staged installer payload is not present")`
	if got := strings.Count(src, skipLine); got != 4 {
		t.Fatalf("payload-absent skip 出现 %d 次, want 4（smoke fallback 先例 + 三个契约测试）: issue #71", got)
	}
	for _, fn := range []string{
		"func TestWritePayloadPreservesExistingOperatorState",
		"func TestWritePayloadSeedsFreshInstall",
		"func TestWritePayloadInstallsTheBundledFFmpeg",
	} {
		i := strings.Index(src, fn)
		if i < 0 {
			t.Errorf("%s 不存在", fn)
			continue
		}
		next := src
		if j := strings.Index(src[i:], "\nfunc "); j >= 0 {
			next = src[i : i+j]
		} else {
			next = src[i:]
		}
		if !strings.Contains(next, skipLine) {
			t.Errorf("%s 没有 payload 缺席的 skip：干净 clone 上会硬失败（issue #71）", fn)
		}
	}
}

// Makefile 半区：`make payload` 目标存在，且 check 的描述不再声称与 CI 完全
// 一致（CI 还跑 JS 语法/真浏览器 smoke/-race/全量 windows 套件）。
func TestMakefileOffersThePayloadTarget(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Skipf("cannot read Makefile: %v", err)
	}
	mk := string(b)
	for _, want := range []string{
		"payload:",
		"installer/build.ps1 -NoData",
		"the same gate CI runs", // must NOT be present any more
	} {
		switch {
		case want == "the same gate CI runs":
			if strings.Contains(mk, want) {
				t.Error("check 目标仍声称『the same gate CI runs』：CI 还跑 JS 语法/smoke/-race/staging，本地没有（issue #71）")
			}
		default:
			if !strings.Contains(mk, want) {
				t.Errorf("Makefile 缺少 %q：可重复的 staging 命令是 #71 的另一半", want)
			}
		}
	}
}
