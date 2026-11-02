package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// 面板写接口的来源闸（issue #103）。
//
// 无 api_key 的部署里，写接口只检查「带没带对密钥」而不看「请求从哪来」：
// 任意网页一次跨站简单 POST（no-cors，连预检都不发）就能改写配置并落盘。
// 闸的三层：Sec-Fetch-Site 命名关系（浏览器不能被指示省略）、Origin 必须
// 是本面板或回环、Host 必须是配置地址或回环（DNS rebinding 关闭）。

func writePanel(gate func(string, ...any) *core.Guard) http.Handler {
	return New(Options{Version: "test", Listen: "127.0.0.1:8893"})
}

func TestCrossSiteWriteIsRejected(t *testing.T) {
	h := New(Options{Version: "test", Listen: "127.0.0.1:8893"})

	// 跨站简单请求的完整形态：no-cors + text/plain + evil Origin。
	req := httptest.NewRequest(http.MethodPost, "/panel/api/config", strings.NewReader(`{"aliases":{"x":"y"}}`))

	req.Host = "127.0.0.1" // 面板写接口的来源闸（issue #103）：测试请求来自本面板。	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site write = %d, want 403 (issue #103): %s", rec.Code, rec.Body.String())
	}

	// Sec-Fetch-Site 直接声明跨站：即便 Origin 伪装成本面板也要拒。
	req2 := httptest.NewRequest(http.MethodPost, "/panel/api/config", strings.NewReader(`{}`))

	req.Host = "127.0.0.1" // 面板写接口的来源闸（issue #103）：测试请求来自本面板。	req2.Header.Set("Origin", "http://127.0.0.1:8893")
	req2.Header.Set("Sec-Fetch-Site", "cross-site")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("cross-site via Sec-Fetch-Site = %d, want 403 (issue #103)", rec2.Code)
	}
}

func TestSameSiteAndToolWritesPass(t *testing.T) {
	h := New(Options{Version: "test", Listen: "127.0.0.1:8893"})

	// 浏览器同源写：Origin = 本面板。
	req := httptest.NewRequest(http.MethodPost, "/panel/api/config", strings.NewReader(`{}`))

	req.Host = "127.0.0.1" // 面板写接口的来源闸（issue #103）：测试请求来自本面板。
	req.Header.Set("Origin", "http://127.0.0.1:8893")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("same-origin write was rejected: %s", rec.Body.String())
	}

	// 工具写（curl / 面板自身 fetch）：无 Origin、无 Sec-Fetch-Site，Host 回环。
	req2 := httptest.NewRequest(http.MethodPatch, "/panel/api/config", strings.NewReader(`{}`))

	req2.Host = "127.0.0.1:8893"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusForbidden {
		t.Fatalf("same-machine tool write was rejected: %s (curl must keep working)", rec2.Body.String())
	}

	// loopback 任意端口（测试里面板端口会动）。
	req3 := httptest.NewRequest(http.MethodPatch, "/panel/api/config", strings.NewReader(`{}`))

	req3.Host = "127.0.0.1:1"
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code == http.StatusForbidden {
		t.Fatalf("loopback-alt-port write was rejected: %s", rec3.Body.String())
	}
}

// DNS rebinding：一个解析到 127.0.0.1 的域名，带着攻击者选的 Host 和无 Origin
// 到达——Host 不是本面板/回环即拒。
func TestRebindedHostIsRejected(t *testing.T) {
	h := New(Options{Version: "test", Listen: "127.0.0.1:8893"})
	req := httptest.NewRequest(http.MethodPatch, "/panel/api/config", strings.NewReader(`{}`))

	req.Host = "attacker.example:8893"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rebinded host = %d, want 403 (issue #103)", rec.Code)
	}
}
