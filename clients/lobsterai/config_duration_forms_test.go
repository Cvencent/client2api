package lobsterai

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 配置块的两种写法（issue #114）。
//
// README 承诺：「Durations accept a Go duration string ("90s") or a bare JSON
// number read as seconds ("cooldown": 300); anything else falls back to the
// default rather than failing the module.」而字段原本是裸 string——裸数字让
// json.Unmarshal 报错，调用方又用 `cfg = Config{}` 收场，于是照着 README 写
// 一个裸数字的代价是**整块配置连同 accounts[] 一起消失**（平台账号池被清空，
// 面板保存时还只回 200）。下面三条钉住：两种写法都收、坏形状只降级该键、
// 任何解析错误都不能吃掉账号池。

func TestDurationAcceptsBothDocumentedForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"Go duration string", `{"cooldown":"90s"}`, 90 * time.Second},
		{"bare number as seconds", `{"cooldown":300}`, 300 * time.Second},
		{"fractional seconds", `{"cooldown":1.5}`, 1500 * time.Millisecond},
		{"millisecond string", `{"cooldown":"500ms"}`, 500 * time.Millisecond},
		{"absent", `{}`, defaultCooldown},
		{"null", `{"cooldown":null}`, defaultCooldown},
	}
	for _, c := range cases {
		cfg, err := parseConfig(json.RawMessage(c.raw))
		if err != nil {
			t.Fatalf("%s: parseConfig(%s) = %v, want no error (the README promises a fallback, not a failure)", c.name, c.raw, err)
		}
		if got := cfg.cooldown(); got != c.want {
			t.Errorf("%s: cooldown(%s) = %v, want %v", c.name, c.raw, got, c.want)
		}
	}
}

// TestBadDurationShapeOnlyLosesThatKnob 钉住「anything else falls back to the
// default rather than failing the module」：bool / 对象 / 数组 / 超量数字都只让
// 这一个键退回默认，同一块里的兄弟键照常生效。
func TestBadDurationShapeOnlyLosesThatKnob(t *testing.T) {
	for _, raw := range []string{
		`{"cooldown":true,"chat_timeout":"2m"}`,
		`{"cooldown":{"s":1},"chat_timeout":"2m"}`,
		`{"cooldown":[1],"chat_timeout":"2m"}`,
		`{"cooldown":1e12,"chat_timeout":"2m"}`,
		`{"cooldown":"not-a-duration","chat_timeout":"2m"}`,
	} {
		cfg, err := parseConfig(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("parseConfig(%s) = %v: a bad duration must not fail the block", raw, err)
		}
		if got := cfg.cooldown(); got != defaultCooldown {
			t.Errorf("%s: cooldown = %v, want the default %v", raw, got, defaultCooldown)
		}
		if got := cfg.chatTimeout(); got != 2*time.Minute {
			t.Errorf("%s: chat_timeout = %v, want 2m (its neighbour must still parse)", raw, got)
		}
	}
}

// TestConfigParseErrorNeverCostsTheAccountPool 钉住第二层兜底：即使真的出现
// 解析错误（将来的某个字段写错、或整块里混了非法值），accounts[] /
// extra_models[] 也必须被救回来。这条不依赖「今天没有这种字段」——它守的是
// 「以后再有也不能吃掉账号池」。
func TestConfigParseErrorNeverCostsTheAccountPool(t *testing.T) {
	// accounts 的形状对，但 max_in_flight 是字符串：int 字段解不了，整块报错。
	raw := json.RawMessage(`{
		"max_in_flight": "two",
		"extra_models": ["lob-probe-a","lob-probe-b"],
		"accounts": [
			{"uid":"probe-uid-1","access_token":"tok-a"},
			{"uid":"probe-uid-2","access_token":"tok-b"}
		]
	}`)
	cfg, err := parseConfig(raw)
	if err == nil {
		t.Fatal("parseConfig 接受了 max_in_flight 的字符串：这条测试的前提（解析失败）没成立")
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("解析失败后只剩下 %d 个账号，期望 2 个：一次配置错误就清空了整个账号池（issue #114）", len(cfg.Accounts))
	}
	if cfg.Accounts[0].UID != "probe-uid-1" || cfg.Accounts[0].AccessToken != "tok-a" {
		t.Errorf("救回的账号内容不对：%+v", cfg.Accounts[0])
	}
	if len(cfg.ExtraModels) != 2 {
		t.Errorf("extra_models 没被救回：%v", cfg.ExtraModels)
	}
	// 错误本身仍要保留：面板需要把它显示出来，不能静默。
	if !strings.Contains(err.Error(), "clients.lobsterai") {
		t.Errorf("错误信息没有带模块前缀：%v", err)
	}
}

// TestDurationFieldRoundTripsAsAString 钉住序列化形状：字段仍写成 Go duration
// 字符串，面板与任何重新序列化配置的地方看到的都是规范形态（不是裸数字）。
func TestDurationFieldRoundTripsAsAString(t *testing.T) {
	cfg, err := parseConfig(json.RawMessage(`{"cooldown":300}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	// 赋给 cfg 后 normalize 不会改时长字段，直接序列化看 wire 形态。
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"cooldown":"300s"`) {
		t.Errorf("裸数字没有归一成 Go duration 字符串：%s", body)
	}
}
