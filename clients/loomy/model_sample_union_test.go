package loomy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// 2026-11-20：/v1/models 只剩 spark-x2.6-low / spark-x2.6-4b 两条。根因不是
// 解析器丢行，而是厂商按账号下发不同目录——同一天实测，13 个活账号看到 6 个
// chat 模型（spark-x2.6、DeepSeek-V4.1-Flash、deepseek-v4-flash-1017、
// GLM-5.3-Flash、qwen3.8-flash、auto），19 个只看到 spark-x2.6-low/-4b 两条；
// 而 fetchModels 在第一个应答成功的账号就返回，目录缓存随首个应答者的权益
// 分组在 6 条和 2 条之间翻转。修法 = 采样多个账号、按模型 id 取并集。
// ---------------------------------------------------------------------------

// narrowModelsBody is the two-row catalogue the narrow entitlement group sees.
const narrowModelsBody = `{"object":"list","data":[` +
	`{"id":"spark-x2.6-low","name":"spark x2.6","type":"chat","context_length":1048576,` +
	`"max_output_tokens":131072,"capabilities":{"reasoning":true,"input_modalities":["text"]}},` +
	`{"id":"spark-x2.6-4b","name":"spark x2.6 4b","type":"chat","context_length":1048576,` +
	`"max_output_tokens":131072,"capabilities":{"reasoning":true,"input_modalities":["text"]}}]}`

// wideModelsBody is the six-row catalogue the wide entitlement group sees,
// trimmed of noise the parser skips anyway.
const wideModelsBody = `{"object":"list","data":[` +
	`{"id":"spark-x2.6","name":"Spark-X2.6 正式版(x2.5)","type":"chat","context_length":1048576,` +
	`"max_output_tokens":131072,"capabilities":{"reasoning":true,"input_modalities":["text"]}},` +
	`{"id":"DeepSeek-V4.1-Flash","name":"DeepSeek V4.1 Flash(x2.0)","type":"chat","context_length":1048576,` +
	`"max_output_tokens":384000,"capabilities":{"reasoning":true,"input_modalities":["text","image"]}},` +
	`{"id":"deepseek-v4-flash-1017","name":"DeepSeek V4.1 Flash 1017(x2.0)","type":"chat","context_length":1048576,` +
	`"max_output_tokens":384000,"capabilities":{"reasoning":true,"input_modalities":["text"]}},` +
	`{"id":"GLM-5.3-Flash","name":"GLM 5.3 Flash(x0.8)","type":"chat","context_length":1048576,` +
	`"max_output_tokens":131072,"capabilities":{"reasoning":true,"input_modalities":["text"]}},` +
	`{"id":"qwen3.8-flash","name":"qwen 3.8 flash（x0.8）","type":"chat","context_length":1000000,` +
	`"max_output_tokens":131072,"capabilities":{"reasoning":true,"input_modalities":["text"]}},` +
	`{"id":"Hy-Image-3.5-preview","name":"Hy image 3.5 preview","type":"image","context_length":100000,` +
	`"capabilities":{"reasoning":false,"input_modalities":["text","image"]}}]}`

// modelsRoutingStub answers each /models request with a different catalogue per
// account token, so a test can place accounts into entitlement groups.
func modelsRoutingStub(perToken map[string]string) *stubTransport {
	return &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path != "/api/v1/models" {
			return jsonResponse(http.StatusOK, `{"code":"000000","data":{}}`)
		}
		body, ok := perToken[r.Header.Get("token")]
		if !ok {
			return jsonResponse(http.StatusOK, `{"code":"100002","desc":"登录已失效，请重新登录"}`)
		}
		return jsonResponse(http.StatusOK, body)
	}}
}

// seedGroup seeds n accounts whose tokens map into perToken groups; the ids are
// given increasing last-used recency is NOT set, so candidates() hands them out
// in pool order (equal lastUsed keeps insertion order via SliceStable).
func seedGroup(t *testing.T, c *Client, tokens []string) {
	t.Helper()
	for i, tok := range tokens {
		seedAccount(t, c, fmt.Sprintf("loomy-%02d", i), tok, 0)
	}
}

func modelIDs(models []core.Model) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if strings.EqualFold(id, want) {
			return true
		}
	}
	return false
}

// TestFetchModelsMergesEntitlementGroups 是主钉子：首个应答者来自窄分组时，
// 目录也必须包含宽分组独有的模型——刷新采样到并集为止，而不是谁先应答谁说了算。
func TestFetchModelsMergesEntitlementGroups(t *testing.T) {
	rt := modelsRoutingStub(map[string]string{
		testToken:    narrowModelsBody,
		testTokenTwo: wideModelsBody,
	})
	c := newTestClient(t, "{}", rt)
	seedGroup(t, c, []string{testToken, testTokenTwo})

	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	ids := modelIDs(models)
	for _, want := range []string{"spark-x2.6-low", "spark-x2.6-4b", "spark-x2.6", "DeepSeek-V4.1-Flash", "GLM-5.3-Flash", "qwen3.8-flash"} {
		if !containsID(ids, want) {
			t.Fatalf("并集丢失 %s：%v", want, ids)
		}
	}
	// 非聊天的 image 行仍然被跳过。
	if containsID(ids, "Hy-Image-3.5-preview") {
		t.Fatalf("image 模型混进了目录：%v", ids)
	}
	// 两个账号都被问到了：一个应答不再终结采样。
	if got := rt.count(); got < 2 {
		t.Fatalf("只请求了 %d 次 /models，采样没有走到第二个账号", got)
	}
}

// TestFetchModelsStopsSamplingWhenStable 钉住成本上界：连续两个应答不再带来新
// id 时提前收手——窄+窄的组合只问 3 次（第 3 次确认稳定后停），不把 cap 5 次
// 烧满。
func TestFetchModelsStopsSamplingWhenStable(t *testing.T) {
	rt := modelsRoutingStub(map[string]string{
		testToken:    narrowModelsBody,
		testTokenTwo: narrowModelsBody,
		"aaaa":       narrowModelsBody,
		"bbbb":       narrowModelsBody,
		"cccc":       narrowModelsBody,
		"dddd":       narrowModelsBody,
	})
	c := newTestClient(t, "{}", rt)
	seedGroup(t, c, []string{testToken, testTokenTwo, "aaaa", "bbbb", "cccc", "dddd"})

	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if ids := modelIDs(models); len(ids) != 2 {
		t.Fatalf("models = %v, want the two spark rows", ids)
	}
	// 第 1 次增长（stable=0），第 2、3 次确认稳定（stable=2）后停；
	// 第 4 个账号不会被问。
	if got := rt.count(); got != 3 {
		t.Fatalf("稳定后未按时收手：/models 被请求了 %d 次（want 3）", got)
	}
}

// TestFetchModelsCapsSamplingRequests 钉住最坏情形上界：即使每个账号都带来新
// id，请求次数也不超过 modelsSampleAccounts。
func TestFetchModelsCapsSamplingRequests(t *testing.T) {
	perToken := map[string]string{}
	var tokens []string
	for i := 0; i < modelsSampleAccounts+3; i++ {
		tok := fmt.Sprintf("tok%02d0123456789abcdef", i)
		tokens = append(tokens, tok)
		perToken[tok] = fmt.Sprintf(
			`{"object":"list","data":[{"id":"m-%02d","name":"m %02d","type":"chat","context_length":1000}]}`, i, i)
	}
	rt := modelsRoutingStub(perToken)
	c := newTestClient(t, "{}", rt)
	seedGroup(t, c, tokens)

	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if got := rt.count(); got != modelsSampleAccounts {
		t.Fatalf("/models 被请求了 %d 次，want cap = %d", got, modelsSampleAccounts)
	}
}

// TestFetchModelsServesPartialWhenLaterProbesFail 钉住降级：已并成的目录必须
// 返回，即使后面的账号探针全失败——部分成功优于空手而归。
func TestFetchModelsServesPartialWhenLaterProbesFail(t *testing.T) {
	rt := modelsRoutingStub(map[string]string{
		testToken: narrowModelsBody,
	})
	c := newTestClient(t, "{}", rt)
	seedGroup(t, c, []string{testToken, testTokenTwo})

	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v（窄目录账号已成功，应返回部分结果）", err)
	}
	if ids := modelIDs(models); len(ids) != 2 {
		t.Fatalf("models = %v, want the narrow group's two rows", ids)
	}
}

// TestFetchModelsSingleAccountUnchanged 是回归钉：只有一个账号时行为与旧实现
// 一致——目录就是那个账号的应答，请求只发生一次。
func TestFetchModelsSingleAccountUnchanged(t *testing.T) {
	rt := modelsRoutingStub(map[string]string{
		testToken: wideModelsBody,
	})
	c := newTestClient(t, "{}", rt)
	seedGroup(t, c, []string{testToken})

	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	ids := modelIDs(models)
	if !containsID(ids, "DeepSeek-V4.1-Flash") || !containsID(ids, "GLM-5.3-Flash") {
		t.Fatalf("单账号目录缺模型：%v", ids)
	}
	if containsID(ids, "Hy-Image-3.5-preview") {
		t.Fatalf("image 模型混进了目录：%v", ids)
	}
	if got := rt.count(); got != 1 {
		t.Fatalf("单账号应只请求 1 次 /models，got %d", got)
	}
}

// TestModelsCacheCarriesMergedCatalogue 端到端对准故障形态：从冷缓存走
// Models()（/v1/models 与面板模型页实际读的那条路径），后台刷新落地后缓存必须
// 携带并集。
func TestModelsCacheCarriesMergedCatalogue(t *testing.T) {
	rt := modelsRoutingStub(map[string]string{
		testToken:    narrowModelsBody,
		testTokenTwo: wideModelsBody,
	})
	c := newTestClient(t, "{}", rt)
	seedGroup(t, c, []string{testToken, testTokenTwo})

	// 冷缓存：Models() 立即回退到内置目录并触发后台刷新；等后台刷新落地。
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if ids := modelIDs(models); !containsID(ids, "deepseek-v4-flash-0731") {
		t.Fatalf("冷缓存应回退到内置目录：%v", ids)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cached, _ := c.cachedModels()
		if len(cached) > 0 {
			models = cached
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	ids := modelIDs(models)
	if !containsID(ids, "DeepSeek-V4.1-Flash") {
		t.Fatalf("缓存里的目录没有并进宽分组模型：%v", ids)
	}
	if !containsID(ids, "spark-x2.6-low") {
		t.Fatalf("缓存里的目录没有并进窄分组模型：%v", ids)
	}
	if got := rt.count(); got < 2 {
		t.Fatalf("后台刷新只请求了 %d 次 /models，采样没有走到第二个账号", got)
	}
}
