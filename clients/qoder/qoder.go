// Package qoder implements the "qoder" client: the Qoder CN desktop coding
// agent (qoder.com.cn), the mainland build of Qoder.
//
// # WHAT THIS MODULE DOES
//
// It owns the account pool and the two reward-facing capabilities the vendor
// really exposes over plain HTTP:
//
//   - the credit ledger (GET /api/v2/quota/usage), rendered as the account's
//     balance; and
//   - the daily "claim 100 credits" activity, which is a read of
//     GET /sash/api/v1/me/campaigns followed by a POST to
//     /sash/api/v1/me/campaigns/{campaignId}/claim.
//
// # WHAT IT DELIBERATELY DOES NOT DO
//
// Qoder CN has no chat API a third-party process can drive.  Its completions and
// its model catalogue both live on https://gateway.qoder.com.cn behind a
// per-request signature minted by the desktop client's native security SDK
// (sgsdk.dll + runtime-info.exe), which is not reproducible from Go.  A request
// without it is answered 403 "Signature invalid" (the /algo routes) or 503 by
// the load balancer (everything else), verified live.  Chat therefore returns
// core.ErrUnsupported rather than pretending, and this module never pretends to
// fetch a model catalogue it cannot read.
//
// The credential is the desktop client's own device token.  This module can read
// it straight out of the client's safeStorage blob (credential_windows.go) or
// take a pasted token, but it never mints one: the device flow belongs to the
// desktop client, and when the vendor stops accepting a token the honest remedy
// is to sign in there and import again.
package qoder

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"client2api/internal/core"
)

// Client implements core.Client for qoder, plus the optional interfaces named
// in the assertion block below.
type Client struct {
	deps core.Deps
	cfg  config
	up   *upstream

	store *store

	// Paths inside Deps.DataDir; empty when no DataDir was supplied, in which
	// case nothing is persisted anywhere and the pool lives only in memory.
	accountsPath string
	statePath    string

	stateMu   sync.Mutex
	lastErr   string
	lastErrAt time.Time
}

// New builds the client.  It never fails for a missing or malformed credential:
// both are runtime conditions reported by Status(), not construction failures.
func New(deps core.Deps) (core.Client, error) {
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		// A typo in one timeout must never take the module off the panel.
		deps.Log("qoder: invalid config, using defaults: %v", err)
		cfg = config{}
	}
	applyEnv(&cfg)

	if deps.DataDir != "" {
		if err := core.EnsureDir(deps.DataDir); err != nil {
			deps.Log("qoder: creating data dir: %v", err)
		}
	}

	c := &Client{deps: deps, cfg: cfg}
	c.setPaths()

	st, err := loadStore(c.accountsPath, c.statePath)
	if err != nil {
		// A corrupt store is reported and the module carries on empty: the
		// operator can re-import, which is better than refusing to start.
		deps.Log("qoder: %v", err)
	}
	c.store = st
	c.store.applyConfig(configuredAccounts(cfg))

	c.up = newUpstream(cfg, deps.HTTPClient, deps.Logf)
	return c, nil
}

func init() { core.Register("qoder", New) }

// setPaths resolves the module's files inside Deps.DataDir.
func (c *Client) setPaths() {
	if c.deps.DataDir == "" {
		return
	}
	c.accountsPath = filepath.Join(c.deps.DataDir, accountsFile)
	c.statePath = filepath.Join(c.deps.DataDir, stateFile)
}

// Name is both the registry key and the routing prefix ("qoder/<model>").
func (c *Client) Name() string { return "qoder" }

// The panel and the scheduler discover capabilities by assertion, so every
// interface this module genuinely supports is asserted here at compile time.
var (
	_ core.Client             = (*Client)(nil)
	_ core.AccountManager     = (*Client)(nil)
	_ core.CredentialImporter = (*Client)(nil)
	_ core.BalanceProvider    = (*Client)(nil)
	_ core.CheckinProvider    = (*Client)(nil)
	_ core.Reviver            = (*Client)(nil)
)

// Models returns the catalogue.  It is a pure function of the configuration --
// there is no cache to refresh and no upstream to poll, because the vendor's own
// catalogue is unreadable from here (see the package comment).
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	return catalogue(c.cfg.modelIDs()), nil
}

// modelIDs is the bare list Status reports.
func (c *Client) modelIDs() []string {
	ids := make([]string, 0, 4)
	for _, m := range catalogue(c.cfg.modelIDs()) {
		ids = append(ids, m.ID)
	}
	return ids
}

// Chat is intentionally unsupported.
//
// The gateway turns this into HTTP 400 invalid_request with the message below,
// which is the honest answer: Qoder CN's inference host demands a signature only
// its desktop client can mint, so a request from here could never succeed, and
// reporting that is better than a 502 that looks like a transient outage.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	return nil, fmt.Errorf("%w: Qoder CN 的对话接口在 gateway.qoder.com.cn 上，需要桌面客户端原生安全 SDK 生成的请求签名，本项目无法复现；"+
		"这个平台目前只能用来管理账号、查看积分和领取每日奖励", core.ErrUnsupported)
}

// Status is deliberately cheap: no network calls and one mutex-guarded
// snapshot.  The panel calls it every ten seconds.
func (c *Client) Status(ctx context.Context) core.Status {
	now := time.Now().UTC()
	st := core.Status{
		Name:      "qoder",
		UpdatedAt: time.Now(),
		Models:    c.modelIDs(),
	}

	accounts := c.store.snapshot()
	records := make([]core.AccountStatus, 0, len(accounts))
	usable, gone := 0, 0
	for i := range accounts {
		records = append(records, accountStatus(&accounts[i], now))
		if accounts[i].selectable(now) {
			usable++
		}
		if accounts[i].dead || accounts[i].expired(now) {
			gone++
		}
	}
	st.Accounts = records

	switch {
	case len(accounts) == 0:
		st.Ready = false
		st.Detail = "还没有凭据：在 Qoder CN 客户端登录后点「从 Qoder CN 导入」，" +
			"或手动粘贴设备令牌（CLIENT2API_QODER_TOKEN）"
	case usable == 0 && gone > 0:
		st.Ready = false
		st.Detail = "令牌已被厂商拒绝或已过期；请在 Qoder CN 客户端重新登录后重新导入"
	case usable == 0:
		st.Ready = false
		st.Detail = "所有账号都被停用或正在冷却"
	default:
		st.Ready = true
		st.Detail = fmt.Sprintf("%d/%d 个账号可用；本模块提供积分查询与每日签到（对话不可用，见 README）",
			usable, len(accounts))
	}

	if msg, at := c.lastFailure(); msg != "" && !st.Ready {
		st.Detail += "；最近一次错误 " + msg + "（" + humanAge(time.Since(at)) + " 前）"
	}
	return st
}

// noteFailure remembers the last upstream failure for Status.  Only the message
// is kept, and it has already been redacted by the caller's path.
func (c *Client) noteFailure(err error) {
	if err == nil {
		return
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.lastErr = redactErr(err)
	c.lastErrAt = time.Now()
}

func (c *Client) lastFailure() (string, time.Time) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.lastErr, c.lastErrAt
}
