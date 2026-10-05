package panel

import (
	"regexp"
	"strings"
	"testing"
)

// TestModelsViewEditsContextAndOutput 钉住「模型页可编辑上下文 / 最大输出」的
// 界面部分。这些都是 index.html 里的字符串拼接，Go 编译器管不到，所以静态钉。
func TestModelsViewEditsContextAndOutput(t *testing.T) {
	src := poolStatsUISource(t)

	// 1. 两个字段都渲染成数字输入框，并带上服务端解析出来的默认值。
	render := poolStatsFuncBody(t, src, "renderModels")
	for _, want := range []string{
		`mdNumCell("context"`,
		`mdNumCell("output"`,
		"m.context_length",
		"m.context_source",
		"m.context_default",
		"m.max_output_tokens",
		"m.max_output_source",
		"m.max_output_default",
	} {
		if !strings.Contains(render, want) {
			t.Errorf("renderModels 没有用 %s 渲染上下文列", want)
		}
	}
	if !strings.Contains(src, `type="number"`) {
		t.Error("上下文 / 最大输出没有渲染成数字输入框")
	}

	// 2. 来源必须逐字段中文化，而不是把 raw source 直接丢给操作员。
	for _, label := range []string{"手动", "上游", "预设", "未公开"} {
		if !strings.Contains(src, label) {
			t.Errorf("来源标签缺少中文 %q", label)
		}
	}
	if !strings.Contains(src, "MD_SRC_LABEL") {
		t.Error("没有 source -> 中文标签的映射表")
	}

	// 3. 每行有「恢复官方默认」，并且它会提交 0 让服务端删掉手动值。
	if !strings.Contains(render, "mdRestore") {
		t.Error("模型行没有恢复默认按钮")
	}
	if !strings.Contains(src, "恢复官方默认") {
		t.Error("恢复默认按钮没有中文标题")
	}
	save := poolStatsFuncBody(t, src, "mdSave")
	if !strings.Contains(save, "e.restore") {
		t.Error("mdSave 没有处理「恢复默认」")
	}
	if !strings.Contains(save, "context_length: 0") || !strings.Contains(save, "max_output_tokens: 0") {
		t.Error("恢复默认必须提交 0 才能删掉手动覆盖")
	}

	// 4. 统一保存按钮存在，并且打到 model_context 接口。
	if !strings.Contains(src, `id="btnMdSave"`) {
		t.Error("模型页没有统一保存按钮 btnMdSave")
	}
	if !strings.Contains(save, `"/panel/api/model_context"`) {
		t.Error("mdSave 没有提交到 /panel/api/model_context")
	}
	if !strings.Contains(src, `$("#btnMdSave").addEventListener("click", mdSave)`) {
		t.Error("保存按钮没有接到 mdSave")
	}

	// 5. 清空输入框 = 提交 0 = 删除该字段的手动值。
	if !strings.Contains(save, "parseInt(e.ctx, 10) || 0") || !strings.Contains(save, "parseInt(e.out, 10) || 0") {
		t.Error("mdSave 没有把空值转成 0（清空即删除）")
	}
	// 只改一半时，另一半原本的手动值必须原样带上，否则会被这次提交清掉。
	if !strings.Contains(save, `it.ctxSrc === "manual"`) || !strings.Contains(save, `it.outSrc === "manual"`) {
		t.Error("mdSave 没有保留未改动那一半的既有手动值")
	}

	// 6. 过滤会重建 tbody，未保存的编辑必须贴回去。
	if !strings.Contains(src, "mdReapplyEdits()") {
		t.Error("过滤重建 tbody 后没有把未保存的编辑贴回去")
	}
}

// TestModelsViewReadsTheMetadataKeysTheGoStructSends 是跨包一致性检查：
// 界面读的字段名必须逐字等于 panel.modelInfo 的 json tag。
func TestModelsViewReadsTheMetadataKeysTheGoStructSends(t *testing.T) {
	src := poolStatsUISource(t)
	body := readFileString(t, "models.go")
	// 只取 modelInfo 结构体那一段，避免把 clientModels 的 tag 也算进来。
	start := strings.Index(body, "type modelInfo struct {")
	end := strings.Index(body, "\n}")
	if start < 0 || end < 0 {
		t.Fatal("models.go has no modelInfo struct")
	}
	structBody := body[start:end]
	tags := map[string]bool{}
	for _, m := range regexp.MustCompile(`json:"([a-z_]+)`).FindAllStringSubmatch(structBody, -1) {
		tags[m[1]] = true
	}
	for _, key := range []string{
		"context_length", "max_output_tokens", "context_source", "max_output_source",
		"context_default", "max_output_default",
	} {
		if !tags[key] {
			t.Errorf("modelInfo 缺少 json tag %q", key)
		}
		if !strings.Contains(src, "m."+key) {
			t.Errorf("renderModels 没有读 m.%s", key)
		}
	}
}
