package zcode

// 面板网页登录的两个上游：国际版（chat.z.ai / api.z.ai）与国内版（bigmodel.cn）。
//
// 背景：国内手机号在国际版登录页上过完人机验证会直接「请求失败」，只有
// bigmodel.cn 那个登录页才收得到验证码。厂商自己的桌面客户端把这两个上游做成
// 两个 OAuth provider（"zai" 与 "bigmodel"），CLI 流程的 /oauth/cli/init 也按
// provider 返回不同的 authorize_url —— 传 "zai" 回来的是
// https://chat.z.ai/api/oauth/authorize?..., 传 "bigmodel" 回来的才是
// https://bigmodel.cn/login?...。
//
// 面板的 realm 选择器就是为「加账号前必须先选服务」的模块准备的（workbuddy 已经
// 在用），所以 zcode 要实现 core.RealmLoginProvider：把 realm 原样当 provider
// 发给 init，并把它记在会话和账号上。
//
// 还有一处必须跟着分叉：国际版轮询回来的 access token 要走 api.z.ai 的
// business-token 交换才能变成 API key，国内版没有这一步（厂商的
// BigModelProviderAdapter.normalizePolledTokenSet 是恒等映射，压根没有
// businessTokenResolver），它的凭据就是轮询直接给的计划 JWT。
//
// 这些全是离线测试：上游由 fakeTransport 应答，凭据存在临时目录里。

import (
	"context"
	"strings"
	"testing"

	"client2api/internal/core"
)

// mustRealmProvider 把 zcode 收窄成 core.RealmLoginProvider，并说清为什么需要它。
func mustRealmProvider(t *testing.T, c *Client) core.RealmLoginProvider {
	t.Helper()
	rp, ok := core.AsRealmLoginProvider(c)
	if !ok {
		t.Fatal("zcode 必须实现 core.RealmLoginProvider：国内手机号只能在 bigmodel 那个登录页登录")
	}
	return rp
}

func TestLoginRealmsCoverBothZhipuServices(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, happyRoutes().transport())

	realms := mustRealmProvider(t, c).LoginRealms(context.Background())
	if len(realms) != 2 {
		t.Fatalf("LoginRealms = %+v, want exactly the two Zhipu services", realms)
	}
	byCode := map[string]core.LoginRealm{}
	for _, r := range realms {
		if strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.Help) == "" {
			t.Errorf("realm %+v needs a name and a help line for the picker", r)
		}
		byCode[r.Code] = r
	}
	if _, ok := byCode[regionBigmodel]; !ok {
		t.Errorf("国内版（bigmodel）realm 缺失：%+v", realms)
	}
	if _, ok := byCode[regionZai]; !ok {
		t.Errorf("国际版（zai）realm 缺失：%+v", realms)
	}
	// 说明文字必须点出这是哪个域名，否则操作员没法判断自己该选哪一个。
	if r := byCode[regionBigmodel]; !strings.Contains(r.Help, "bigmodel") {
		t.Errorf("国内版说明没有提 bigmodel：%q", r.Help)
	}
}

func TestStartLoginRealmSendsThePickedProvider(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	transport := happyRoutes().transport()
	c := env.client(t, transport)

	st, err := c.StartLoginRealm(context.Background(), regionBigmodel)
	if err != nil {
		t.Fatalf("StartLoginRealm(bigmodel): %v", err)
	}
	if transport.count() != 1 {
		t.Fatalf("start must send exactly one request, sent %d", transport.count())
	}
	if got := strings.TrimSpace(transport.bodyAt(0)); got != `{"provider":"bigmodel"}` {
		t.Errorf("init body = %s, want provider=bigmodel so the vendor returns the bigmodel.cn page", got)
	}
	if st.Realm != regionBigmodel {
		t.Errorf("state realm = %q, want %q so the panel can label the sign-in", st.Realm, regionBigmodel)
	}
}

func TestStartLoginDefaultsToZaiUnlessConfigured(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want string
	}{
		{"unset keeps the historical default", `{"auto_discover":false}`, regionZai},
		{"configured mainland", `{"auto_discover":false,"oauth_provider":"bigmodel"}`, regionBigmodel},
		{"case and padding are ignored", `{"auto_discover":false,"oauth_provider":" BIGMODEL "}`, regionBigmodel},
		{"an unknown value falls back to zai", `{"auto_discover":false,"oauth_provider":"nonsense"}`, regionZai},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPanelEnv(t, tc.cfg)
			transport := happyRoutes().transport()
			c := env.client(t, transport)

			st, err := c.StartLogin(context.Background())
			if err != nil {
				t.Fatalf("StartLogin: %v", err)
			}
			if got := strings.TrimSpace(transport.bodyAt(0)); got != `{"provider":"`+tc.want+`"}` {
				t.Errorf("init body = %s, want provider=%s", got, tc.want)
			}
			if st.Realm != tc.want {
				t.Errorf("state realm = %q, want %q", st.Realm, tc.want)
			}
		})
	}
}

// bigmodel 的轮询结果不需要（也不能）走 api.z.ai 的兑换：国内版没有 business
// token 这一步，凭据就是轮询直接给的计划 JWT。这条用例同时钉住「没有多打一次
// 兑换请求」和「账号被记在 bigmodel 服务上」。
func TestBigmodelLoginStoresThePlanCredentialWithoutTheZaiExchange(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	transport := happyRoutes().transport()
	c := env.client(t, transport)

	st, err := c.StartLoginRealm(context.Background(), regionBigmodel)
	if err != nil {
		t.Fatalf("StartLoginRealm: %v", err)
	}
	final, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if final.State != core.LoginSuccess {
		t.Fatalf("state = %q (%s), want success", final.State, final.Message)
	}
	// init + poll，仅此两条。多出来的就是误走的 z.ai 兑换。
	if transport.count() != 2 {
		t.Errorf("a mainland sign-in made %d requests, want 2 (init + poll; no business-token exchange)", transport.count())
	}

	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("stored %d accounts, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Fields["kind"] != kindJWT {
		t.Errorf("kind = %v, want the polled plan JWT (bigmodel has no api-key exchange)", rec.Fields["kind"])
	}
	if rec.Fields["provider"] != providerBigmodel {
		t.Errorf("provider = %v, want %q", rec.Fields["provider"], providerBigmodel)
	}
	if rec.Fields["realm"] != regionBigmodel {
		t.Errorf("realm = %v, want %q so the account row shows which service it belongs to", rec.Fields["realm"], regionBigmodel)
	}
}

// 国际版这条老路必须一字不变：仍然走完整的兑换，存下来的还是 API key。
func TestZaiLoginStillExchangesForAnAPIKey(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	transport := happyRoutes().transport()
	c := env.client(t, transport)

	st, err := c.StartLoginRealm(context.Background(), regionZai)
	if err != nil {
		t.Fatalf("StartLoginRealm: %v", err)
	}
	if final, err := c.PollLogin(context.Background(), st.SessionID); err != nil {
		t.Fatalf("PollLogin: %v", err)
	} else if final.State != core.LoginSuccess {
		t.Fatalf("state = %q (%s), want success", final.State, final.Message)
	}
	if transport.count() < 5 {
		t.Errorf("the z.ai sign-in made %d requests, want the full exchange walk", transport.count())
	}
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("stored %d accounts, want 1", len(recs))
	}
	if rec := recs[0]; rec.Fields["kind"] != kindAPIKey || rec.Fields["provider"] != providerZai {
		t.Errorf("record = %+v, want a zai api-key", rec.Fields)
	}
}

// 配置里手填（或导入）的账号也要带 realm 字段，否则账号池里两条通道看起来一模一样，
// 操作员分不清哪条是国内、哪条是国际。
func TestAccountRecordsNameTheirRealm(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false,"accounts":[`+
		`{"id":"zai-key","provider":"zai","mode":"api_key","api_key":"`+testAPIKey+`"},`+
		`{"id":"bm-key","provider":"bigmodel","mode":"api_key","api_key":"`+testOtherKey+`"}]}`)
	c := env.client(t, &fakeTransport{})

	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	want := map[string]string{"zai-key": regionZai, "bm-key": regionBigmodel}
	for _, rec := range recs {
		if got := rec.Fields["realm"]; got != want[rec.ID] {
			t.Errorf("account %s realm = %v, want %q", rec.ID, got, want[rec.ID])
		}
	}
}

// 面板把 realm 原样发回来，未知值不能被静默当成国际版——那会把凭据存到操作员
// 没选的服务上。
func TestStartLoginRealmRejectsAnUnknownRealm(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	transport := happyRoutes().transport()
	c := env.client(t, transport)

	_, err := c.StartLoginRealm(context.Background(), "atlantis")
	if err == nil {
		t.Fatal("an unknown realm must be refused, not silently defaulted")
	}
	if transport.count() != 0 {
		t.Error("an unknown realm must fail before any vendor request")
	}
}
