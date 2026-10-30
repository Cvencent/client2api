package panel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// fakeTester 是一个实现了 ModelTester 的模块：记录最后一次被要求测的模型。
type fakeTester struct {
	testedModel  string
	testedWithID string
	calls        int
	failWith     string
}

func (f *fakeTester) Name() string { return "faketester" }

func (f *fakeTester) Models(context.Context) ([]core.Model, error) { return nil, nil }

func (f *fakeTester) Chat(context.Context, *core.ChatRequest) (core.Stream, error) {
	return nil, core.ErrUnsupported
}

func (f *fakeTester) Status(context.Context) core.Status { return core.Status{Name: "faketester"} }

func (f *fakeTester) AccountFields(context.Context) []core.FieldSpec { return nil }
func (f *fakeTester) Accounts(context.Context) ([]core.AccountRecord, error) {
	return []core.AccountRecord{{ID: "a1", Enabled: true, State: "ready"}}, nil
}
func (f *fakeTester) AddAccount(context.Context, core.AccountSpec) (core.AccountRecord, error) {
	return core.AccountRecord{}, errors.New("not used")
}
func (f *fakeTester) RemoveAccount(context.Context, string) error           { return nil }
func (f *fakeTester) SetAccountEnabled(context.Context, string, bool) error { return nil }
func (f *fakeTester) RefreshAccount(context.Context, string) ([]core.RefreshResult, error) {
	return nil, nil
}

// TestAccount 是「默认模型」的旧路径。
func (f *fakeTester) TestAccount(_ context.Context, id string) (core.TestResult, error) {
	f.calls++
	f.testedWithID = id
	f.testedModel = ""
	if f.failWith != "" {
		return core.TestResult{}, errors.New(f.failWith)
	}
	return core.TestResult{OK: true, Model: "module-default", Reply: "pong"}, nil
}

// TestAccountModel 是操作员选模型的新路径。
func (f *fakeTester) TestAccountModel(_ context.Context, id, model string) (core.TestResult, error) {
	f.calls++
	f.testedWithID = id
	f.testedModel = model
	if f.failWith != "" {
		return core.TestResult{}, errors.New(f.failWith)
	}
	if model == "" {
		model = "module-default"
	}
	// 一个账号能跑某模型就 OK，否则是一次正常失败（不是 error）。
	if model == "unsupported-model" {
		return core.TestResult{Model: model, Error: "this account cannot serve that model"}, nil
	}
	return core.TestResult{OK: true, Model: model, Reply: "pong"}, nil
}

// plainAccountClient 实现了 AccountManager 但**不**实现 ModelTester：它的探测
// 模型是写死的，面板不该给它一个模型下拉。
// 刻意用「单独的字段」而不是内嵌 fakeTester：内嵌会把 fakeTester 的
// TestAccountModel 一起带过来，于是这个类型就「意外地」实现了 ModelTester，
// 负控也就测不到东西了。
type plainAccountClient struct {
	base  fakeTester
	tests int
}

func (p *plainAccountClient) Name() string                                 { return p.base.Name() }
func (p *plainAccountClient) Models(context.Context) ([]core.Model, error) { return nil, nil }
func (p *plainAccountClient) Chat(context.Context, *core.ChatRequest) (core.Stream, error) {
	return nil, core.ErrUnsupported
}
func (p *plainAccountClient) Status(context.Context) core.Status {
	return core.Status{Name: "faketester"}
}

func (p *plainAccountClient) TestAccount(context.Context, string) (core.TestResult, error) {
	p.tests++
	return core.TestResult{OK: true, Model: "fixed-model"}, nil
}

func (p *plainAccountClient) AccountFields(context.Context) []core.FieldSpec { return nil }

func (p *plainAccountClient) Accounts(context.Context) ([]core.AccountRecord, error) {
	return []core.AccountRecord{{ID: "a1", Enabled: true, State: "ready"}}, nil
}
func (p *plainAccountClient) AddAccount(context.Context, core.AccountSpec) (core.AccountRecord, error) {
	return core.AccountRecord{}, nil
}
func (p *plainAccountClient) RemoveAccount(context.Context, string) error           { return nil }
func (p *plainAccountClient) SetAccountEnabled(context.Context, string, bool) error { return nil }
func (p *plainAccountClient) RefreshAccount(context.Context, string) ([]core.RefreshResult, error) {
	return nil, nil
}

// modPanel 起一个真实的 panel handler（走 New()，包含鉴权中间件），只挂一个
// 假模块。这样断言的是「HTTP 请求带 model → 模块收到 model」整条链路，
// 而不是某一行代码的写法。
func modPanel(t *testing.T, clients ...core.Client) http.Handler {
	t.Helper()
	reg := core.NewRegistry()
	for _, c := range clients {
		reg.Add(c)
	}
	return New(Options{Registry: reg, Version: "test", Listen: "127.0.0.1:0"})
}

// assertNotModelTester 是一条负控：如果哪天有人把 ModelTester 并回 AccountManager，
// 这个编译期断言会失败，提醒他先想清楚「探测模型写死的模块」该怎么办。
func TestModelTesterIsASeparateInterface(t *testing.T) {
	var c core.Client = &plainAccountClient{}
	if _, ok := c.(core.ModelTester); ok {
		t.Fatal("plainAccountClient 不该实现 ModelTester：它探测的是固定模型")
	}
	var d core.Client = &fakeTester{}
	if _, ok := d.(core.ModelTester); !ok {
		t.Fatal("fakeTester 应该实现 ModelTester")
	}
}

// TestCapabilitiesReportTestModel 钉住能力位：只有实现了 ModelTester 的模块才
// 报 test_model=true，面板据此决定给不给下拉。
func TestCapabilitiesReportTestModel(t *testing.T) {
	ctx := context.Background()
	if caps := core.CapabilitiesOf(ctx, &fakeTester{}); !caps.TestModel {
		t.Errorf("ModelTester 模块的能力位没有报 test_model：%+v", caps)
	}
	if caps := core.CapabilitiesOf(ctx, &plainAccountClient{}); caps.TestModel {
		t.Errorf("非 ModelTester 模块不该报 test_model：%+v", caps)
	}
}

// TestTestModelReachesTheModule 是这条链路的核心断言：面板收到的 model 必须
// 原样到达模块，并且结果里回显同一个 model——操作员是选了模型才点的，结果
// 里不出现那个模型就无法判断测的是不是他问的那件事。
func TestTestModelReachesTheModule(t *testing.T) {
	mod := &fakeTester{}
	srv := modPanel(t, mod)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/clients/faketester/accounts/a1/test",
		strings.NewReader(`{"model":"vendor/model-x"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if mod.testedModel != "vendor/model-x" {
		t.Errorf("模块收到的模型 = %q，期望 vendor/model-x", mod.testedModel)
	}
	if mod.testedWithID != "a1" {
		t.Errorf("模块收到的账号 = %q，期望 a1", mod.testedWithID)
	}
	if !strings.Contains(rec.Body.String(), "vendor/model-x") {
		t.Errorf("响应里没有回显被测的模型：%s", rec.Body.String())
	}
}

// TestEmptyModelKeepsTheOldBehaviour：不给模型时走原来的 TestAccount，
// 也就是模块自己的默认探测模型。这是「这个按钮以前怎么工作」的回归保护。
func TestEmptyModelKeepsTheOldBehaviour(t *testing.T) {
	mod := &fakeTester{}
	srv := modPanel(t, mod)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/clients/faketester/accounts/a1/test", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if mod.calls != 1 {
		t.Errorf("模块被调用了 %d 次，期望 1 次", mod.calls)
	}
	if mod.testedModel != "" {
		t.Errorf("空模型时不该把某个具体模型塞给模块，收到 %q", mod.testedModel)
	}
	if !strings.Contains(rec.Body.String(), "module-default") {
		t.Errorf("响应里没有回显默认模型：%s", rec.Body.String())
	}
}

// TestUnsupportedModelIsAFailedProbeNotARequest：账号跑不了某模型时，那是
// 一次「正常的失败探测」，不是 400。操作员问了一个具体问题，答案是「不行」。
func TestUnsupportedModelIsAFailedProbeNotARequest(t *testing.T) {
	mod := &fakeTester{}
	srv := modPanel(t, mod)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/clients/faketester/accounts/a1/test",
		strings.NewReader(`{"model":"unsupported-model"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d（期望 200：这是失败的探测，不是坏请求），body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ok":false`) {
		t.Errorf("失败探测应当 ok=false：%s", body)
	}
	if !strings.Contains(body, "cannot serve that model") {
		t.Errorf("模块给出的理由没有透出去：%s", body)
	}
}

// TestModelIsIgnoredByModulesThatDoNotSupportIt：把一个模型发给不支持选模型的
// 模块时，面板必须退回原来的 TestAccount，而不是假装用过那个模型。
func TestModelIsIgnoredByModulesThatDoNotSupportIt(t *testing.T) {
	mod := &plainAccountClient{}
	srv := modPanel(t, mod)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/clients/faketester/accounts/a1/test",
		strings.NewReader(`{"model":"vendor/model-x"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if mod.tests != 1 {
		t.Errorf("模块的 TestAccount 被调用 %d 次，期望 1 次", mod.tests)
	}
	if strings.Contains(rec.Body.String(), "vendor/model-x") {
		t.Errorf("不支持选模型的模块不该在结果里回显操作员选的模型：%s", rec.Body.String())
	}
}
