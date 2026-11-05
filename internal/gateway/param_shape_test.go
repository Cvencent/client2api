package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 钉住调用方参数形状校验（issue #215）：
//
//	max_tokens ≤ 0 曾走完「显式指定」分支却不写进上游请求——网关既不听
//	调用方的也不替它定；temperature/top_p 越界曾原样透传，厂商 400 被折成
//	upstream_error，运维去看平台而真正的错在请求里。解码层拒绝：400 带明说
//	哪个参数不合法的信封，不是 upstream_error。
func TestCallerShapeErrorsAreRejectedAtDecode(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"max_tokens zero", `{"model":"m1","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`, "max_tokens=0"},
		{"max_tokens negative", `{"model":"m1","max_tokens":-5,"messages":[{"role":"user","content":"hi"}]}`, "max_tokens=-5"},
		{"temperature below", `{"model":"m1","temperature":-3,"messages":[{"role":"user","content":"hi"}]}`, "temperature=-3"},
		{"temperature above", `{"model":"m1","temperature":99,"messages":[{"role":"user","content":"hi"}]}`, "temperature=99"},
		{"top_p below", `{"model":"m1","top_p":-1,"messages":[{"role":"user","content":"hi"}]}`, "top_p=-1"},
		{"top_p above", `{"model":"m1","top_p":2,"messages":[{"role":"user","content":"hi"}]}`, "top_p=2"},
	}
	for _, tc := range cases {
		stats := NewStats()
		srv := newTestServer(t, &testClient{name: "t", events: usageEvents(1, 1)}, stats, nil)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d (%s), want 400（调用方形状错误必须在解码层拒绝，issue #215）", tc.name, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.wantMsg) {
			t.Errorf("%s 的错误文案 = %s, want it to name %s", tc.name, rec.Body.String(), tc.wantMsg)
		}
		if strings.Contains(rec.Body.String(), "upstream_error") {
			t.Errorf("%s 被报成 upstream_error：调用方自己的参数错不该指向平台（issue #215）", tc.name)
		}
	}
	// 合法值照常 200。
	stats := NewStats()
	srv := newTestServer(t, &testClient{name: "t", events: usageEvents(1, 1)}, stats, nil)
	okBody := `{"model":"m1","max_tokens":100,"temperature":0.7,"top_p":0.9,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(okBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid params = %d (%s), want 200", rec.Code, rec.Body.String())
	}
}
