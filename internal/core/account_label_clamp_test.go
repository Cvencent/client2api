package core

import "testing"

// ---------------------------------------------------------------------------
// 账号 label 的共享钳制（issue #232）。
//
// label 逐字进账号表、逐拍进 /panel/api/overview —— 无界字符串让一次粘贴
// 变成每 5 秒 211 KB 的固定带宽成本。120 是既有口径（configwrite 的
// maxAccountNoteRunes、codearts/minimaxcode 的 maxLabelRunes），钳制逻辑
// 收敛到 ClampLabel 一处，全部模块 intake 走它。
// ---------------------------------------------------------------------------

func TestClampLabelCapsLengthAndStripsControl(t *testing.T) {
	got := ClampLabel("  " + string([]rune{'a', 0x07, 'b'}) + "  ")
	if got != "ab" {
		t.Errorf("ClampLabel = %q, want %q（trim + 控制字符剥离）", got, "ab")
	}
	long := make([]rune, 500)
	for i := range long {
		long[i] = '字'
	}
	clamped := ClampLabel(string(long))
	if len([]rune(clamped)) != 120 {
		t.Errorf("ClampLabel 长度 = %d, want 120（无界粘贴就是 #232 的 211KB/拍）", len([]rune(clamped)))
	}
	if ClampLabel("", "  ", "second") != "second" {
		t.Error("ClampLabel 的多候选回退没有跳过空串")
	}
	if ClampLabel() != "" {
		t.Error("ClampLabel 无参数应返回空串")
	}
}

func TestAccountSpecLabelOfPrefersLabelThenFields(t *testing.T) {
	spec := AccountSpec{Label: "  outer  ", Fields: map[string]string{"label": " inner ", "nickname": "nick"}}
	if got := spec.LabelOf("label"); got != "outer" {
		t.Errorf("LabelOf = %q, want the spec label (first non-empty wins)", got)
	}
	spec.Label = ""
	if got := spec.LabelOf("label"); got != "inner" {
		t.Errorf("LabelOf = %q, want the field label when the spec label is empty", got)
	}
	if got := spec.LabelOf("nickname"); got != "nick" {
		t.Errorf("LabelOf = %q, want the named field fallback", got)
	}
	if got := spec.LabelOf(); got != "" {
		t.Errorf("LabelOf 无字段 = %q, want 空", got)
	}
}
