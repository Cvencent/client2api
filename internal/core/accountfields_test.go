package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// FieldSpec.Advanced 是「这个字段默认收起来」的标记：面板按它渲染高级折叠区。
// 缺省必须是 false（老模块的字段继续平铺），线上 JSON 里也必须省略，否则每一个
// 没有高级字段的模块都会白带一个 advanced:false。
func TestFieldSpecAdvancedIsOptionalOnTheWire(t *testing.T) {
	plain, err := json.Marshal(FieldSpec{Key: "provider", Label: "服务商", Type: "text"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(plain), "advanced") {
		t.Errorf("没有高级标记的字段不该输出 advanced：%s", plain)
	}

	advanced, err := json.Marshal(FieldSpec{Key: "base_url", Label: "接口地址", Type: "text", Advanced: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(advanced), `"advanced":true`) {
		t.Errorf("高级字段必须在线上带着 advanced:true：%s", advanced)
	}

	var got FieldSpec
	if err := json.Unmarshal([]byte(`{"key":"models","advanced":true}`), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Advanced {
		t.Error("面板传回的 advanced 标记没有被解析")
	}
	if (FieldSpec{}).Advanced {
		t.Error("FieldSpec 的零值必须是「不折叠」")
	}
}
