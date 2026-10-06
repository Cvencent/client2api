package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// CheckinAction.Channels 的序列化契约。
//
// 背景：签到动作是客户端级的（CheckinProvider.CheckinActions），但有些动作只对
// 某一种凭据通道成立。zcode 的活动套餐走计划账单接口，只认计划 JWT；同一个账号
// 的编码计划 API key 是它的兄弟通道，账号行上和 JWT 并列，但那一行根本领不了。
// 模块需要在动作上声明它限定在哪些通道（值就是账号记录里的 fields.kind，比如
// "jwt" / "api-key"），面板才能按行收窄。这个字段必须能被 JSON 带出去给面板。
//
// 另一头同样重要：没有声明通道的动作（其余所有模块的每日签到）JSON 里不能多出
// 这个键，否则面板会误以为它们也被限定了通道。
// ---------------------------------------------------------------------------

func TestCheckinActionCarriesItsChannelScope(t *testing.T) {
	b, err := json.Marshal(CheckinAction{
		ID:       "claim",
		Label:    "领取活动套餐",
		Channels: []string{"jwt"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"channels":["jwt"]`) {
		t.Fatalf("序列化结果里没有 channels：%s", b)
	}
}

func TestCheckinActionOmitsChannelsWhenUnscoped(t *testing.T) {
	b, err := json.Marshal(CheckinAction{ID: "daily", Label: "每日签到"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "channels") {
		t.Fatalf("没有限定通道的动作不该带上 channels 键：%s", b)
	}
	// 省略也要能反序列化回来：老面板 / 老缓存读到不带这个键的动作时，Channels
	// 保持 nil，语义就是「每一行都出现」，和以前完全一样。
	var back CheckinAction
	if err := json.Unmarshal([]byte(`{"id":"daily","label":"每日签到"}`), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Channels) != 0 {
		t.Fatalf("缺省动作的 Channels = %v, want empty", back.Channels)
	}
}
