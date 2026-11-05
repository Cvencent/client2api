package panel

import (
	"strings"
	"testing"
)

// 钉住两条保存契约（issue #199 / #200）。

// #199：排程间隔在乘法之前预算上限——「约 585 年」曾回绕成「每 5.3 秒一拍」
// 并直接产生上游请求。30 天上限覆盖一切真实巡检节奏。
func TestScheduleIntervalRejectsAnOverflowingValue(t *testing.T) {
	for _, raw := range []map[string]any{
		{"recovery_probe_seconds": 18446744079.0},  // wraps to ~5.3s
		{"recovery_probe_seconds": 100000000000.0}, // wraps to another value
		{"recovery_probe_minutes": 18446744079.0},  // minutes path
		{"balance_refresh_minutes": 307445735.0},   // the reported sweep case
	} {
		om := map[string]any{}
		for k, v := range raw {
			om[k] = v
		}
		if _, _, err := scheduleIntervalValue("schedule", stemOf(om), om); err == nil {
			t.Errorf("interval %v was accepted：回绕的间隔会直接变成秒级上游请求（issue #199）", raw)
		} else if !strings.Contains(err.Error(), "too large") {
			t.Errorf("interval %v error = %v, want the 'too large' wording（负回绕的「必须大于零」曾误导排查）", raw, err)
		}
	}
	// 合法值照常通过。
	if d, ok, err := scheduleIntervalValue("schedule", "recovery_probe", map[string]any{"recovery_probe_seconds": 14400.0}); err != nil || !ok || d != 14400_000_000_000 {
		t.Fatalf("14400s = %v, ok=%v, err=%v, want the plain duration", d, ok, err)
	}
}

func stemOf(om map[string]any) string {
	for k := range om {
		if strings.HasSuffix(k, "_seconds") {
			return strings.TrimSuffix(k, "_seconds")
		}
		if strings.HasSuffix(k, "_minutes") {
			return strings.TrimSuffix(k, "_minutes")
		}
	}
	return ""
}

// #200：模型组/别名引用的 client 段必须已注册。
func TestReferencesToUnregisteredClientsAreRejected(t *testing.T) {
	c := &fakeClient{name: "kilocode"}
	reg := registryOf(c)
	// model_groups：成员指向拼错的客户端名 → 400。
	cfg := map[string]any{
		"model_groups": map[string]any{
			"gpt-6-luna-cheap": map[string]any{
				"members": []any{"gpt6client/gpt-6-luna"},
			},
		},
	}
	err := validateModelGroups(cfg, cfg, reg)
	if err == nil {
		t.Fatal("a group member naming an unregistered client was accepted：会被 /v1/models 广播并 404（issue #200）")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error = %v, want the not-registered wording naming the client", err)
	}
	// 对照：真实客户端名照常通过。
	ok := map[string]any{
		"real-group": map[string]any{
			"members": []any{"kilocode/cohere/north-mini-code:free"},
		},
	}
	if err := validateModelGroups(ok, ok, reg); err != nil {
		t.Fatalf("a group on a real client was rejected: %v", err)
	}
	// aliases：目标指向未注册客户端 → 400。
	aliasCfg := map[string]any{
		"aliases": map[string]any{"probe-ok": "nosuchclient/deepseek"},
	}
	if err := validateConfig(aliasCfg, aliasCfg, reg); err == nil {
		t.Fatal("an alias to an unregistered client was accepted（issue #200）")
	}
}
