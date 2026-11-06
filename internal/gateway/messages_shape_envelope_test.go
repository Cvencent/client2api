package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// /v1/messages 的参数形状校验与错误信封（issue #231）。
//
// #215 的三条调用方形状校验只落在 /v1/chat/completions 与 /v1/responses：
// Anthropic 入口上 max_tokens≤0 曾静默从上游请求体里消失（既不听调用方也不
// 定预算），越界采样参数被折成 upstream_error 指向上游。信封半边：共享路由
// 的四个上游失败点无视 writeProtocolError，把 OpenAI 扁平信封漏给只认
// {"type":"error","error":{…}} 的 Claude Code。
// ---------------------------------------------------------------------------

// A. max_tokens≤0 在解码层 400（Anthropic 拼写），上游一个请求都收不到。
func TestAMessagesRejectsNonPositiveMaxTokens(t *testing.T) {
	var logs bytes.Buffer
	srv := newTestServerWithLog(t, &testClient{name: "t", events: usageEvents(1, 1)}, &logs)

	for _, mt := range []string{"0", "-5"} {
		body := `{"model":"m1","max_tokens":` + mt + `,"messages":[{"role":"user","content":"hi"}]}`
		rec := postMessages(t, srv, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("max_tokens=%s: status = %d, want 400 (issue #231)", mt, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"type":"error"`) {
			t.Errorf("max_tokens=%s: 不是 Anthropic 信封：%s", mt, rec.Body.String()[:min(200, rec.Body.Len())])
		}
		if !strings.Contains(rec.Body.String(), "not a valid output budget") {
			t.Errorf("max_tokens=%s: 错误文案没有点名输出预算：%s", mt, rec.Body.String())
		}
	}
}

// B. 越界 temperature / top_p 同样在解码层 400，不再折成 upstream_error。
func TestBMessagesRejectsOutOfRangeSamplingParams(t *testing.T) {
	var logs bytes.Buffer
	srv := newTestServerWithLog(t, &testClient{name: "t", events: usageEvents(1, 1)}, &logs)

	for _, tc := range []struct{ field, val string }{
		{"temperature", "99"},
		{"top_p", "-1"},
	} {
		body := `{"model":"m1","max_tokens":10,"` + tc.field + `":` + tc.val + `,"messages":[{"role":"user","content":"hi"}]}`
		rec := postMessages(t, srv, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s=%s: status = %d, want 400 (issue #231)", tc.field, tc.val, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "outside the supported range") {
			t.Errorf("%s=%s: 文案没有点名范围：%s", tc.field, tc.val, rec.Body.String())
		}
	}
}

// C. 合法请求不受影响：translate 不拒绝、max_tokens 原样保留。
func TestCLegalMessagesRequestStillTranslates(t *testing.T) {
	req, err := translateAnthropic(t, `{"model":"m1","max_tokens":10,"temperature":0.7,"top_p":0.9,"messages":[{"role":"user","content":"hi"}]}`)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 10 {
		t.Errorf("max_tokens = %v, want 10 preserved", req.MaxTokens)
	}
}

// D. 上游层失败的信封是 Anthropic 形状：顶层必须带 type:"error"。
// 路由 refusal（模型不在白名单）走 protocolError 早已正确；这里钉的是
// 上游/账号池层（dispatch 的 writeUpstreamError 四连）不再漏 OpenAI 形状。
func TestDUpstreamFailureOnMessagesUsesAnthropicEnvelope(t *testing.T) {
	var logs bytes.Buffer
	// 没有账号的模块 → ErrNotConfigured → 账号池层拒绝（四个失败点之一）。
	srv := newTestServerWithLog(t, &testClient{name: "t", chatErr: context.Canceled}, &logs)

	rec := postMessages(t, srv, `{"model":"m1","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Errorf("上游层失败不是 Anthropic 信封：%s（issue #231）", body[:min(300, len(body))])
	}
	var top struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body[:min(300, len(body))])
	}
	if top.Type != "error" {
		t.Errorf("顶层 type = %q, want \"error\"（OpenAI 扁平信封漏到了 /v1/messages，issue #231）", top.Type)
	}
}

// E. 静态钉：anthropic 的 translate 必须有三条校验；dispatch 的四个上游
// 失败点必须走 routeInput.upstreamError（可被协议覆盖）。
func TestEMessagesValidationAndEnvelopeWiring(t *testing.T) {
	src, err := os.ReadFile("anthropic.go")
	if err != nil {
		t.Skipf("cannot read anthropic.go: %v", err)
	}
	a := string(src)
	for _, want := range []string{
		"max_tokens=%d is not a valid output budget",
		"temperature=%v is outside the supported range 0..2",
		"top_p=%v is outside the supported range 0..1",
		"writeUpstreamErr: s.writeAnthropicUpstreamError",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("anthropic.go 缺少 %q（issue #231）", want)
		}
	}
	d, err := os.ReadFile("dispatch.go")
	if err != nil {
		t.Skipf("cannot read dispatch.go: %v", err)
	}
	dd := string(d)
	if n := strings.Count(dd, "s.writeUpstreamError(w, client, hintCtx,"); n != 0 {
		t.Errorf("dispatch.go 仍有 %d 处直接调 writeUpstreamError：协议信封会被绕过（issue #231）", n)
	}
	if strings.Count(dd, "in.upstreamError(w, client, hintCtx,") < 4 {
		t.Error("dispatch.go 的上游失败点没有全部走 routeInput.upstreamError（issue #231）")
	}
}

// postMessages posts a body to /v1/messages and returns the recorder, the way
// chat() does for the OpenAI endpoint.
func postMessages(t *testing.T, srv *http.Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}
