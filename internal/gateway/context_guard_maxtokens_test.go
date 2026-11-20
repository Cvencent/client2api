package gateway

import (
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// issue #265：显式 max_tokens 把「输入+输出」顶过窗口时，护栏仍按 32K 预留
// 放行——必然失败的请求白付流内 TTFB 才被厂商拒绝（radeoncloud/GLM-5.3-Flash
// 262144 窗口两天 15 次）。修法 = prepareCandidateAttempt 在请求前判「输入估算
// + 显式 max_tokens > 窗口」，合成与厂商同语义的 context_window_exceeded 400；
// 注入的 max_tokens（网关自己按目录填的）豁免——厂商能正常服务这类请求。
// ---------------------------------------------------------------------------

// TestExplicitMaxTokensOverWindowIsRefusedBeforeTheWire 钉住主链路：显式
// max_tokens 128000 + 139K 输入打 262144 窗口的模型，第一次调用前就吃本地 400
// （context_window_exceeded），厂商零调用。
func TestExplicitMaxTokensOverWindowIsRefusedBeforeTheWire(t *testing.T) {
	c := &windowClient{name: "radeon", window: 262144}
	usage := NewUsageStore(10)
	srv := candidateTrimServer(t, usage, c)

	// 40 turn × 12000 chars ≈ 141K 估算 token（issue 里 139619 的实测形状，
	// 估算器 3.4 chars/token）；显式 max_tokens 128000 → 总和 ~269K，超
	// 262144 窗口且远超安全余量。
	body := strings.Replace(candidateTrimBody(40, 12000), `"stream":false}`, `"stream":false,"max_tokens":128000}`, 1)
	rr := chat(t, srv, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want a local 400 before any vendor call: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "context_window_exceeded") {
		t.Fatalf("the refusal lost its machine-readable code: %s", rr.Body.String())
	}
	if c.calls != 0 {
		t.Fatalf("vendor calls = %d, want 0: the refusal must not cost a round trip (issue #265)", c.calls)
	}
}

// TestExplicitMaxTokensInsideWindowStillServes 反向钉：输入 + 显式
// max_tokens 在窗口内的请求照常放行——本地预判只拦「必然失败」的。
func TestExplicitMaxTokensInsideWindowStillServes(t *testing.T) {
	c := &windowClient{name: "radeon", window: 262144}
	srv := candidateTrimServer(t, NewUsageStore(10), c)

	body := strings.Replace(candidateTrimBody(20, 3000), `"stream":false}`, `"stream":false,"max_tokens":32000}`, 1)
	rr := chat(t, srv, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if c.calls < 1 {
		t.Fatalf("vendor calls = %d, want >=1: a fitting request must reach the vendor", c.calls)
	}
}

// TestInjectedMaxTokensIsExemptFromThePreRefusal 钉住豁免腿：网关自己注入的
// max_tokens（131072 级）不触发预判——厂商按「输入 + 注入值」实际能服务时
// （或注入值本就来自目录），白拒等于把能成的请求掐死。
func TestInjectedMaxTokensIsExemptFromThePreRefusal(t *testing.T) {
	c := &windowClient{name: "radeon", window: 262144}
	srv := candidateTrimServer(t, NewUsageStore(10), c)

	// 无 max_tokens 的请求走注入路径；139K 级输入 + 注入值（若目录给了
	// 131072）在厂商判窗下可能超，但那是 #62 的流内学习链的事，不是本地预判。
	body := candidateTrimBody(40, 3000)
	rr := chat(t, srv, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if c.calls < 1 {
		t.Fatalf("vendor calls = %d, want >=1: an injected max_tokens must never trip the pre-refusal", c.calls)
	}
}

// TestExplicitMaxTokensAtTheEdgeStillGoesToTheVendor 钉住余量腿：恰好压线的
// 显式 max_tokens（溢出没超过安全余量）仍去厂商——估算器有误差，压线的请求
// 可能真被服务，本地抢答只会错杀。
func TestExplicitMaxTokensAtTheEdgeStillGoesToTheVendor(t *testing.T) {
	// 262144 窗口；输入 ~78K（40 turn × 2000 chars ≈ 78K 估算），显式
	// max_tokens 184000 → 总和 ~262K，溢出只有几百 token——在 8% 安全余量内。
	c := &windowClient{name: "radeon", window: 262144}
	srv := candidateTrimServer(t, NewUsageStore(10), c)

	body := strings.Replace(candidateTrimBody(40, 2000), `"stream":false}`, `"stream":false,"max_tokens":184000}`, 1)
	rr := chat(t, srv, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if c.calls < 1 {
		t.Fatalf("vendor calls = %d, want >=1: an edge request must still be tried, not guessed against", c.calls)
	}
}
