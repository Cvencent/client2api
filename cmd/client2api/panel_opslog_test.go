package main

import (
	"strings"
	"testing"
)

// 面板诊断必须接到**进程日志器**上，而不只是内存环（issue #96）。
//
// 环容量 2000 行，被每请求的 chat 行占满，真实部署上几分钟滚一遍；#96 要的
// 「一周量级的慢响应样本」只有落在可轮转回看的 client2api.log 里才攒得住。
// internal/panel 的单测钉住「诊断会同时写两个面」，这条钉住**进程真的接上了
// 那个文件面**——只改 panel 而忘了这里的话，面板单测全绿、线上依然零命中。
func TestThePanelIsGivenTheDurableDiagnosticsSink(t *testing.T) {
	src := repoFile(t, "cmd/client2api/main.go")

	// 进程侧必须把 logger 交给面板，且必须是逐字写入（诊断行已脱敏，可能含 '%'，
	// 当成格式串会把它吃掉）。
	if !strings.Contains(src, "logger.Output(") {
		t.Error("面板诊断没有接到进程日志器：文件日志里仍然不会有慢响应/panic/写失败（issue #96）")
	}

	// 接线点必须在 panel.New 的 Options 里。
	i := strings.Index(src, "panel.New(panel.Options{")
	if i < 0 {
		t.Fatal("找不到 panel.New(panel.Options{...}) 的构造点")
	}
	j := strings.Index(src[i:], "\n\t})")
	if j < 0 {
		t.Fatal("面板 Options 构造没有找到收尾")
	}
	opts := src[i : i+j]
	if !strings.Contains(opts, "OpsLog:") {
		t.Error("Options 里没有 OpsLog：面板的诊断落盘面没接上（issue #96）")
	}
	// Logs 仍然要在：环是面板实时日志页的来源，落盘是回看面，两者不可互相替代。
	if !strings.Contains(opts, "Logs:") {
		t.Error("Options 里丢了 Logs：面板实时日志页会失去来源")
	}
}
