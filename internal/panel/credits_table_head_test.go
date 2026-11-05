package panel

import (
	"strings"
	"testing"
)

// 钉住积分构成明细表的表头契约（issue #212）：
//
//	第二张 caption 曾被拼进 <tr> 里——HTML 解析器把它当表格第二个 caption、
//	结束该行，<th> 被自动生成的 <tbody> 收走，<thead> 变空行：表头整行滚出
//	视野、:1786 的 sticky 规则整体失效。同次提交还机械写入了 11 处重复
//	scope="col" scope="col"。
func TestCreditsTableHasARealThead(t *testing.T) {
	src := poolStatsUISource(t)

	// 模板段：caption 唯一、在 thead 之前；th 不带重复 scope。
	if strings.Contains(src, "<caption class=\\\"sr-only\\\">积分构成</caption>") {
		t.Error("旧的第二张 caption 还在模板里（issue #212）")
	}
	tpl := strings.Index(src, `积分构成（全部账号的批次明细）`)
	if tpl < 0 {
		t.Fatal("新的唯一 caption 不见了")
	}
	seg := src[tpl : tpl+600]
	if strings.Contains(seg, `scope="col" scope="col"`) || strings.Contains(seg, `scope=\"col\" scope=\"col\"`) {
		t.Error("credits 表头仍有重复 scope 属性：机械替换的痕迹没清干净（issue #212）")
	}
	// 全文件：重复 scope 属性清零（三处 11 个）。
	if strings.Contains(src, `scope=\"col\" scope=\"col\"`) || strings.Contains(src, `scope="col" scope="col"`) {
		t.Error("重复 scope 属性仍存在于源码（issue #212）")
	}
	// th 必须紧跟 thead 内（caption 不得出现在 <tr> 里）。
	if !strings.Contains(seg, "<thead><tr>") && !strings.Contains(seg, "<thead>\n<tr>") {
		// 模板是单行拼接，<thead><tr> 应连续出现
		t.Error("credits 表的 <thead><tr> 结构丢失")
	}
}
