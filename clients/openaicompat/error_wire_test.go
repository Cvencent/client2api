package openaicompat

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// #239：厂商把模型路由到 Gemini 原生接口、用原生解析器的错误回答 OpenAI 形状
// 的请求——「contents is required」是线索本体，文案必须点名线格式不匹配并把
// 排障方向指向厂商路由，而不是一条无名的 500。
// ---------------------------------------------------------------------------

func TestAWireMismatchIsNamedInTheMessage(t *testing.T) {
	c := &Client{}
	err := c.classifyUpstream("acct-1", errors.New(`status_code=500, contents is required (request id: 202611071020556630967638268d9d6KFEw5NL0)`))
	if err == nil {
		t.Fatal("no error returned")
	}
	msg := err.Error()
	for _, want := range []string{
		"contents is required",
		"线格式不匹配",
		"Gemini 原生接口",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message 缺 %q：%s", want, msg)
		}
	}
	// 反向钉：账号不得被标记 invalid（同一 key 的其它模型还是 OpenAI 线）。
	if strings.Contains(msg, "invalid") {
		t.Errorf("线不匹配被误伤成凭据问题：%s", msg)
	}
}

func TestBOtherServerErrorsStayUntouched(t *testing.T) {
	c := &Client{}
	err := c.classifyUpstream("acct-1", errors.New("status_code=500, upstream is unhealthy"))
	if err == nil {
		t.Fatal("no error returned")
	}
	if strings.Contains(err.Error(), "线格式不匹配") {
		t.Errorf("非线不匹配错误被误标：%s", err.Error())
	}
	_ = context.Background
}
