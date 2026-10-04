package gateway

import (
	"context"
	"io"
	"log"
	"net/http"
	"testing"

	"client2api/internal/core"
)

// limitsClient is a testClient that also answers core.ModelLimitsProvider.
//
// It is a separate type on purpose: testClient itself must keep NOT having the
// method, because "a module that never opted in" is a case several existing
// tests depend on, and the whole change is additive.
type limitsClient struct {
	*testClient
	// limits is keyed by the resolved upstream id, exactly as the real modules
	// key their catalogues.
	limits map[string]int
	// calls counts consultations, so a test can prove the gateway does not pay
	// for a lookup it has no use for.
	calls int
}

func (c *limitsClient) ModelMaxOutputTokens(_ context.Context, model string) (int, bool) {
	c.calls++
	n, ok := c.limits[model]
	return n, ok
}

func newLimitsServer(t *testing.T, c core.Client) *http.Server {
	t.Helper()
	reg := core.NewRegistry()
	reg.Add(c)
	return NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
	})
}

const noMaxTokensBody = `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`

// TestTheGatewayFillsTheAdvertisedOutputBudget is the fix itself: a caller that
// named no budget gets the one the vendor advertises, rather than the module's
// own flat default (4096 for zcode, 32000 for qwenwork) truncating a model that
// advertises far more.
func TestTheGatewayFillsTheAdvertisedOutputBudget(t *testing.T) {
	base := &testClient{name: "mod", catalogue: []core.Model{{ID: "m1"}}}
	c := &limitsClient{testClient: base, limits: map[string]int{"m1": 128000}}
	srv := newLimitsServer(t, c)

	if rec := chat(t, srv, noMaxTokensBody); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if base.seen == nil {
		t.Fatal("the request never reached the module")
	}
	if base.seen.MaxTokens == nil {
		t.Fatal("the gateway left max_tokens unset despite an advertised budget")
	}
	if got := *base.seen.MaxTokens; got != 128000 {
		t.Fatalf("max_tokens = %d, want the advertised 128000", got)
	}
}

// TestAnExplicitMaxTokensIsNeverSecondGuessed: the caller's number is a
// decision, not a hint.  The module must not even be asked.
func TestAnExplicitMaxTokensIsNeverSecondGuessed(t *testing.T) {
	base := &testClient{name: "mod", catalogue: []core.Model{{ID: "m1"}}}
	c := &limitsClient{testClient: base, limits: map[string]int{"m1": 128000}}
	srv := newLimitsServer(t, c)

	body := `{"model":"m1","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if base.seen == nil || base.seen.MaxTokens == nil {
		t.Fatal("the caller's max_tokens did not survive translation")
	}
	if got := *base.seen.MaxTokens; got != 100 {
		t.Fatalf("max_tokens = %d, want the caller's 100", got)
	}
	if c.calls != 0 {
		t.Fatalf("the gateway consulted the module %d times for a request that already had a budget", c.calls)
	}
}

// TestMaxCompletionTokensCountsAsExplicit pins that the fill happens after the
// wire translation, not before it: max_completion_tokens is folded into
// max_tokens by toCoreRequest, so a request carrying only that spelling is
// already satisfied and must not be overwritten by the advertised budget.
func TestMaxCompletionTokensCountsAsExplicit(t *testing.T) {
	base := &testClient{name: "mod", catalogue: []core.Model{{ID: "m1"}}}
	c := &limitsClient{testClient: base, limits: map[string]int{"m1": 128000}}
	srv := newLimitsServer(t, c)

	body := `{"model":"m1","max_completion_tokens":256,"messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if base.seen == nil || base.seen.MaxTokens == nil {
		t.Fatal("max_completion_tokens did not reach the module")
	}
	if got := *base.seen.MaxTokens; got != 256 {
		t.Fatalf("max_tokens = %d, want max_completion_tokens' 256", got)
	}
	if c.calls != 0 {
		t.Fatalf("the gateway consulted the module %d times for a request that already had a budget", c.calls)
	}
}

// TestAModuleThatDeclinesLeavesTheRequestUncapped: "cannot say" must send no
// cap, never an invented one.  A cap that is too high is a vendor rejection and
// one that is too low truncates silently, so the only safe answer is a number
// the vendor published.
func TestAModuleThatDeclinesLeavesTheRequestUncapped(t *testing.T) {
	base := &testClient{name: "mod", catalogue: []core.Model{{ID: "m1"}}}
	c := &limitsClient{testClient: base, limits: map[string]int{"another-model": 8192}}
	srv := newLimitsServer(t, c)

	if rec := chat(t, srv, noMaxTokensBody); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if base.seen == nil {
		t.Fatal("the request never reached the module")
	}
	if base.seen.MaxTokens != nil {
		t.Fatalf("max_tokens = %d, want it left unset when the module declines", *base.seen.MaxTokens)
	}
	if c.calls != 1 {
		t.Fatalf("the module was consulted %d times, want exactly 1", c.calls)
	}
}

// TestAModuleWithoutTheInterfaceLeavesTheRequestUncapped is the additivity
// guarantee: minimaxcode and trae do not implement ModelLimitsProvider, and
// their behaviour must be exactly what it was before this change.
func TestAModuleWithoutTheInterfaceLeavesTheRequestUncapped(t *testing.T) {
	base := &testClient{name: "mod", catalogue: []core.Model{{ID: "m1"}}}
	srv := newLimitsServer(t, base)

	if rec := chat(t, srv, noMaxTokensBody); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if base.seen == nil {
		t.Fatal("the request never reached the module")
	}
	if base.seen.MaxTokens != nil {
		t.Fatalf("max_tokens = %d, want it left unset for a module without the interface", *base.seen.MaxTokens)
	}
}

// TestTheFillUsesTheResolvedUpstreamIDNotTheAlias is the subtle one.  The
// module keys its catalogue on its own model id, so a lookup made with the
// alias the caller typed would always miss -- and a miss is invisible, because
// it looks exactly like a module that cannot say.
func TestTheFillUsesTheResolvedUpstreamIDNotTheAlias(t *testing.T) {
	base := &testClient{name: "mod", catalogue: []core.Model{{ID: "m1"}}}
	c := &limitsClient{testClient: base, limits: map[string]int{"m1": 128000}}
	reg := core.NewRegistry()
	reg.Add(c)
	reg.AddAlias("fast", "m1")
	srv := NewServer(Options{Registry: reg, Version: "test", Logger: log.New(io.Discard, "", 0)})

	body := `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if base.seen == nil {
		t.Fatal("the request never reached the module")
	}
	if base.seen.Model != "m1" {
		t.Fatalf("the module saw model %q, want the resolved upstream id m1", base.seen.Model)
	}
	if base.seen.MaxTokens == nil {
		t.Fatal("the alias resolved but the advertised budget was not filled")
	}
	if got := *base.seen.MaxTokens; got != 128000 {
		t.Fatalf("max_tokens = %d, want the advertised 128000", got)
	}
}
