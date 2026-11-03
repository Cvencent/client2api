package openaicompat

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

// 积分读数必须跨重启存活（issue #126）。
//
// 现场：boot 后**第一枚余额 sweep 之前**先到的 chat 401 被写成 invalid →
// 行显示「账号异常 / 重新导入」。操作员点一下「余额」，同一账号立刻翻成
// 「冷却中」——同一凭据，状态跟着**读数**走，而读数当时只在内存里，重启即空。
//
// 判别信号只能是「这个账号有没有过读数」（#104 定下的语义，未改动），所以
// 修法是把读数**落盘**：重启后 veto 仍能回答，chat 早于 sweep 也不再误判。
//
// 判据：同一个 DataDir 上重建 source（模拟重启），读数与 veto 都还在。

// officeAcePremiumSourceIn builds the same source in a caller-owned DataDir, so
// a test can drop it and construct a second one over the same files -- that is
// what a restart looks like to this module.
func officeAcePremiumSourceIn(t *testing.T, srv *httptest.Server, dataDir string) *officeAceSource {
	t.Helper()
	s, err := newSource(officeAceSourceID, core.SourceConfig{BaseURL: srv.URL + "/v2"}, core.Deps{
		DataDir:    dataDir,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	o := &officeAceSource{source: s, eps: officeAceEndpoints{
		ClawBase: srv.URL, AuthBase: srv.URL, IAMBase: srv.URL, KeystoneBase: srv.URL,
		ClientID: "test-client",
	}}
	o.loadOfficeAcePoints()
	o.enableDesktopModelSelection()
	o.bindAuthExhaustedHook()
	return o
}

// A. 读数落盘后，重启出来的实例仍持有否决权（这正是 boot 早于 sweep 时缺的东西）。
func TestOfficeAceCreditReadingSurvivesARestart(t *testing.T) {
	var chatCalls atomic.Int32
	srv := officeAceChatServer(t, officeAceDrainedSubscriptionJSON, &chatCalls)
	defer srv.Close()

	dataDir := t.TempDir()
	first := officeAcePremiumSourceIn(t, srv, dataDir)
	id := officeAceAddChatAccount(t, first, srv)

	// One real reading (the sweep would have done this).
	if _, err := first.AccountBalance(context.Background(), id, 0); err != nil {
		t.Fatalf("balance: %v", err)
	}
	if !first.officeAceRefusalIsCredits(id) {
		t.Fatal("读数之后 veto 没有生效")
	}
	if _, err := os.Stat(filepath.Join(dataDir, officeAcePointsFileName)); err != nil {
		t.Fatalf("读数没有落盘：%v（重启后 veto 会答不出来，issue #126）", err)
	}

	// Restart: a brand-new source over the same DataDir, before ANY read.
	second := officeAcePremiumSourceIn(t, srv, dataDir)
	if !second.officeAceRefusalIsCredits(id) {
		t.Fatal("重启后 veto 失效：boot 早于 sweep 时 chat 的 auth 401 又会被写成「账号异常」（issue #126）")
	}
}

// B. 重启后、sweep 之前到达的 chat 401 必须落冷却而不是 invalid —— 这是操作员
//
//	报障的原始场景，也是「状态跟着读数走、不该跟着 boot 时序走」的直接判据。
func TestChatBeforeTheFirstSweepDoesNotMarkACreditedAccountInvalid(t *testing.T) {
	var chatCalls atomic.Int32
	srv := officeAceChatServer(t, officeAceDrainedSubscriptionJSON, &chatCalls)
	defer srv.Close()

	dataDir := t.TempDir()
	first := officeAcePremiumSourceIn(t, srv, dataDir)
	id := officeAceAddChatAccount(t, first, srv)
	if _, err := first.AccountBalance(context.Background(), id, 0); err != nil {
		t.Fatalf("balance: %v", err)
	}

	// Restart, then the very first thing that happens is a chat 401.
	second := officeAcePremiumSourceIn(t, srv, dataDir)
	if err := officeAceChatOnce(t, second.source); err == nil {
		t.Fatal("chat against a refused key unexpectedly succeeded")
	}
	records, err := second.Accounts(context.Background())
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v, want one account", records)
	}
	if got := records[0].State; got == "invalid" {
		t.Fatalf("boot 后 sweep 之前的 chat 401 又把账号标成「账号异常」(state=%q)："+
			"读数明明在盘上，veto 必须能回答（issue #126）", got)
	}
}

// C. 从未读过余额的账号仍走死 key 路径 —— #104 划下的边界不能因为落盘而放宽。
func TestAnAccountNeverReadStillKeepsTheDeadKeyPath(t *testing.T) {
	var chatCalls atomic.Int32
	srv := officeAceChatServer(t, officeAceDrainedSubscriptionJSON, &chatCalls)
	defer srv.Close()

	o := officeAcePremiumSourceIn(t, srv, t.TempDir())
	officeAceAddChatAccount(t, o, srv)

	if err := officeAceChatOnce(t, o.source); err == nil {
		t.Fatal("chat against a refused key unexpectedly succeeded")
	}
	records, err := o.source.Accounts(context.Background())
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if got := records[0].State; got != "invalid" {
		t.Fatalf("从未读过余额的账号 state=%q，want invalid：死 key 路径必须保留（issue #104）", got)
	}
}

// D. invalid 的写入必须留痕（issue #126 的第二个子面）。
func TestMarkingAnAccountInvalidLeavesATrace(t *testing.T) {
	var chatCalls atomic.Int32
	srv := officeAceChatServer(t, officeAceDrainedSubscriptionJSON, &chatCalls)
	defer srv.Close()

	var logged strings.Builder
	s, err := newSource("relay-trace", core.SourceConfig{BaseURL: srv.URL + "/v1"}, core.Deps{
		DataDir:    t.TempDir(),
		HTTPClient: srv.Client(),
		Logf:       func(format string, args ...any) { logged.WriteString(format + "\n") },
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	if _, err := s.addAccount(context.Background(), core.AccountSpec{
		ID: "trace-1", Label: "trace-1",
		Fields: map[string]string{"api_key": "k", "models": "m"},
	}, srv.URL+"/v2"); err != nil {
		t.Fatalf("add account: %v", err)
	}
	// Drive one auth refusal through the source's own failure path.
	// noteFailure matches on accountBaseLocked, which for a non-OfficeAce source
	// is the SOURCE's own base (the row's per-account base is only consulted for
	// OfficeAce endpoints) -- so this passes the configured base, not /v2.
	s.noteFailure("trace-1", "k", srv.URL+"/v1", &core.Failure{
		Kind: core.FailureAuth, Status: http.StatusUnauthorized,
		Err: errors.New("invalid api key"),
	})

	if !strings.Contains(logged.String(), "marking account") {
		t.Fatalf("写 invalid 没有留痕（issue #126：三个 boot 的 invalid 标记完全无日志）：\n%s", logged.String())
	}
}

var _ = io.Discard
var _ = time.Now
