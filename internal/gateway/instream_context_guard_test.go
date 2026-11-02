package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// 流内到达的上下文超限 400 走完整条护栏（issue #62）。
//
// vendor 可以接受 POST、开出流、吊上几分钟，然后把「你的 prompt 太长」放进一个
// error frame。这条路上的错误过去直接进 s.fail，用的是模块自己分类出来的 kind
// （openaicompat 是 kindClient → FailureOther），于是护栏存在的三件事一件都没做：
// vendor 亲口报的窗口没被学到、刚被 vendor 推翻的 probe 裁决仍然生效（继续按
// 496 089 裁）、调用方拿到一个没有任何上下文语义的 502 upstream_error，于是同一
// 个体型的请求被反复重发，每轮烧 3-5 分钟。
//
// 下面三条钉住：窗口学到、probe lease 被消耗、线上语义变成 context_window_exceeded。

// instreamContextClient 在流内吐出一句 OpenAI 官方措辞的上下文超限。
type instreamContextClient struct {
	name string
	seen *core.ChatRequest
	// events lets a test make the same client succeed.
	events []core.Event
}

func (c *instreamContextClient) Name() string { return c.name }

func (c *instreamContextClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "glm-5.3-flash", OwnedBy: c.name, Extra: map[string]any{
		"context_length": int64(507093),
	}}}, nil
}

func (c *instreamContextClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.seen = req
	if len(c.events) > 0 {
		return &fakeStream{events: c.events}, nil
	}
	// The vendor accepted the request and refuses inside the stream, in the
	// phrasing the report collected verbatim.
	return &fakeStream{events: []core.Event{{
		Type: core.EventError,
		Err: core.Fail(c.name, "a1", core.FailureOther, http.StatusBadGateway,
			fmt.Errorf("This model's maximum context length is 262144 tokens. However, you requested 128000 output tokens and your prompt contains at least 134145 input tokens")),
	}}}, nil
}

func (c *instreamContextClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

func newInstreamServer(t *testing.T, c core.Client, learnedPath string) *server {
	t.Helper()
	o := Options{
		Registry:           core.NewRegistry(),
		Version:            "test",
		Logger:             log.New(io.Discard, "", 0),
		LearnedWindowsPath: learnedPath,
		Stats:              NewStats(),
		Usage:              NewUsageStore(0),
		SessionPlatforms:   NewSessionPlatforms(),
	}
	o.Registry.Add(c)
	s := &server{
		opts:             o,
		started:          time.Now(),
		stats:            o.Stats,
		sessionPlatforms: o.SessionPlatforms,
		estimates:        newEstimateCalibrationStore("", nil),
	}
	s.probes = openProbeStore(filepath.Join(t.TempDir(), "probes.json"), s.opts.Logger.Printf)
	s.learnedWindows = openLearnedWindowStore(learnedPath, s.opts.Logger.Printf)
	s.compaction = newCompactionSigner("", s.opts.Logger.Printf)
	s.handler = s.routes()
	return s
}

// TestInStreamContextRefusalIsLearned 钉住第一件事：vendor 报的窗口必须被学到。
func TestInStreamContextRefusalIsLearned(t *testing.T) {
	c := &instreamContextClient{name: "rzol"}
	s := newInstreamServer(t, c, "")

	body := `{"model":"rzol/glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rr := chatHandler(t, s, body)
	if rr.Code == http.StatusOK {
		t.Fatalf("流内超限却回了 200：%s", rr.Body.String())
	}
	got := s.learnedWindows.get("rzol", "glm-5.3-flash")
	if got != 262144 {
		t.Fatalf("流内报出的窗口没被学到：learned = %d, want 262144（issue #62）", got)
	}
}

// TestInStreamContextRefusalSaysContextWindowExceeded 钉住第三件事：线上语义。
//
// 调用方要能从错误里读出「这段历史太长」，才会去压缩；一个裸的 upstream_error
// 只会让同一体型再来一轮。
func TestInStreamContextRefusalSaysContextWindowExceeded(t *testing.T) {
	c := &instreamContextClient{name: "rzol"}
	s := newInstreamServer(t, c, "")

	body := `{"model":"rzol/glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rr := chatHandler(t, s, body)

	var env struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是 JSON：%v: %s", err, rr.Body.String())
	}
	if env.Error.Code != "context_window_exceeded" {
		t.Fatalf("线上 code = %q, want context_window_exceeded（issue #62：否则调用方不会压缩，同一体型无限重发）", env.Error.Code)
	}
	if env.Error.Type != "invalid_request_error" {
		t.Errorf("线上 type = %q, want invalid_request_error（与模块 return 路径同一口径）", env.Error.Type)
	}
}

// TestInStreamContextRefusalSpendsTheProbeLease 钉住第二件事：被 vendor 推翻的
// probe 裁决必须被消耗，不能再当 trim floor。
func TestInStreamContextRefusalSpendsTheProbeLease(t *testing.T) {
	c := &instreamContextClient{name: "rzol"}
	s := newInstreamServer(t, c, "")
	// Seed the accepted probe the report observed: 496 089 tokens accepted.
	s.probes.recordAccepted("rzol", "glm-5.3-flash", 496089, 496089)

	body := `{"model":"rzol/glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rr := chatHandler(t, s, body)
	if rr.Code == http.StatusOK {
		t.Fatalf("流内超限却回了 200：%s", rr.Body.String())
	}

	// The lease is spent: an accepted verdict must no longer be served.  It is
	// deliberately not asserted as "cleared", because the store may keep the
	// refusal record -- what must not remain is an ACCEPTED floor.
	rec, ok := s.probes.get("rzol", "glm-5.3-flash")
	if ok && rec.Outcome == probeOutcomeAccepted {
		t.Fatalf("probe 裁决仍是 accepted：vendor 刚推翻它，它却还在当 trim floor（issue #62）")
	}
}

// TestLearnedWindowOutranksAnAcceptedProbeFloor 钉住建议方向 2：两个 store 打架
// 时 vendor 说的话赢。
func TestLearnedWindowOutranksAnAcceptedProbeFloor(t *testing.T) {
	c := &bunnyClient{name: "bunny", realWindow: 262144}
	s := newGuardServer(t, c, "")
	s.probes.recordAccepted("bunny", "cn:space-bunny", 496089, 496089)
	s.learnedWindows.put("bunny", "cn:space-bunny", 262144)

	got := s.probeAdjustedBudget(context.Background(), c, "cn:space-bunny", 0, 0)
	if got > 262144 {
		t.Fatalf("trim 预算 = %d，超过 vendor 亲口说的 262144：accepted probe 的 floor 压过了 learned（issue #62）", got)
	}
}

// TestTrimLogPrintsTheBudgetItObeyed 钉住附带发现 A：日志行不许再显示
// 「to fit 0 tokens」。
func TestTrimLogPrintsTheBudgetItObeyed(t *testing.T) {
	raw, err := os.ReadFile("context_guard.go")
	if err != nil {
		t.Fatalf("读不到 context_guard.go：%v", err)
	}
	src := string(raw)
	if strings.Contains(src, `"context: trimmed %d oldest message(s) for %s/%s to fit %d tokens (about %d -> %d)"`) {
		t.Fatal("trim 日志仍打印 effectiveContextWindow：该模型三处全空时会显示「to fit 0 tokens」，与真实预算脱钩（issue #62 附带发现 A）")
	}
	if !strings.Contains(src, "s.trimBudget(ctx, client, model, maxOutput, limit)") {
		t.Error("trim 日志没有使用真实预算")
	}
}

// TestMeasuredSizePatternsReadOpenAISentence 钉住附带发现 B：OpenAI 官方句型的
// 两个数字都要能解析出来。
func TestMeasuredSizePatternsReadOpenAISentence(t *testing.T) {
	msg := "This model's maximum context length is 262144 tokens. However, you requested 128000 output tokens and your prompt contains at least 134145 input tokens"
	sizes := parseRefusalSizes(msg)
	if sizes.Limit != 262144 {
		t.Errorf("Limit = %d, want 262144", sizes.Limit)
	}
	if sizes.Observed != 134145 {
		t.Fatalf("Observed = %d, want 134145：`requested N output tokens` 句型被 output 隔断，比例重试永远走不到（issue #62 附带发现 B）", sizes.Observed)
	}
}
