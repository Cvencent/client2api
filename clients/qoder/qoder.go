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
// It also drives the two inference routes the CN desktop client's own agent
// uses on https://gateway.qoder.com.cn:
//
//   - GET  /algo/api/v2/model/list        -- the chat model catalogue; and
//   - POST /algo/api/v2/service/pro/sse/agent_chat_generation -- the streamed
//     completion.
//
// Both gateway routes require a per-request COSY signature: an AES-encrypted,
// RSA-signed identity envelope plus an MD5 digest over the request path and
// body.  The route names, the envelope and the signing scheme were read out of
// the CN desktop client's own bundle and confirmed live (see cosy.go,
// gateway.go).
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
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"client2api/internal/core"
	"client2api/internal/smscap"
)

// Client implements core.Client for qoder, plus the optional interfaces named
// in the assertion block below.
type Client struct {
	deps core.Deps
	cfg  config
	up   *upstream

	store *store

	// Model catalogue cache. Models is called on every panel refresh and must
	// stay cheap; RefreshModels is the explicit synchronous vendor fetch.
	modelMu         sync.Mutex
	models          []core.Model
	modelsAt        time.Time
	modelsAttemptAt time.Time
	modelsLoading   bool

	// inFlight is the number of chat streams currently open.  Chat feeds it
	// and PoolStats reports it, so the panel shows the real process state
	// rather than a second bookkeeping copy.
	inFlight atomic.Int64

	// taskMu guards the short-lived campaign board cache.  The task centre
	// asks for the board on every draw, so a brief cache keeps one panel open
	// from turning into a stream of vendor calls.
	taskMu        sync.Mutex
	taskBoard     []core.TaskInfo
	taskAccountID string
	taskBoardAt   time.Time

	// COSY signing sessions are derived once per credential and reused, because
	// the identity envelope is part of the vendor's session state.
	sessMu   sync.Mutex
	sessions map[string]sessionEntry

	// affinity pins a conversation to the account that first served it, so
	// the second turn does not walk to the least-recently-used credential.
	// It is nil-safe, so a Client built without New still serves requests.
	affinity *core.Affinity

	httpOnce sync.Once
	ownHTTP  *http.Client

	// Paths inside Deps.DataDir; empty when no DataDir was supplied, in which
	// case nothing is persisted anywhere and the pool lives only in memory.
	accountsPath string
	statePath    string

	stateMu   sync.Mutex
	lastErr   string
	lastErrAt time.Time

	// loginMu guards the in-flight browser-login sessions and the cached
	// device identity.  A login that survives a restart is a login nobody can
	// reason about, so only the machine id is persisted.
	loginMu        sync.Mutex
	logins         map[string]*loginSession
	machineIDCache string

	// smsRot spreads consecutive rentals across provinces so one region
	// does not take repeated traffic.
	smsRot smscap.Rotator

	// autoMu guards the in-flight auto-login jobs.  Like the login
	// sessions they are memory-only: a run that survives a restart is a run
	// nobody can reason about.
	autoMu   sync.Mutex
	autoJobs map[string]*autoJob
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
	c.sessions = make(map[string]sessionEntry)
	c.affinity = core.NewAffinity(0)
	c.affinity.StartGC()
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
	_ core.Client              = (*Client)(nil)
	_ core.AccountManager      = (*Client)(nil)
	_ core.CredentialImporter  = (*Client)(nil)
	_ core.LoginProvider       = (*Client)(nil)
	_ core.BalanceProvider     = (*Client)(nil)
	_ core.CheckinProvider     = (*Client)(nil)
	_ core.ModelRefresher      = (*Client)(nil)
	_ core.Reviver             = (*Client)(nil)
	_ core.PackageProvider     = (*Client)(nil)
	_ core.TaskProvider        = (*Client)(nil)
	_ core.TaskClaimer         = (*Client)(nil)
	_ core.TaskAutoRunner      = (*Client)(nil)
	_ core.HealthProvider      = (*Client)(nil)
	_ core.PoolStatsReporter   = (*Client)(nil)
	_ core.ModelLimitsProvider = (*Client)(nil)
	_ core.SMSProvider         = (*Client)(nil)
	_ core.AutoLoginProvider   = (*Client)(nil)
)

// Models returns the catalogue
//
// Models answers from the cached vendor catalogue and falls back to the
// compiled-in chat list on a cold cache. Registry.Resolve calls Models for bare
// model names, so this path never performs a network round trip.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	cached, at := c.cachedModels()
	if len(cached) > 0 {
		if time.Since(at) >= c.cfg.modelsTTL() {
			c.refreshModelsAsync()
		}
		return cached, nil
	}
	c.refreshModelsAsync()
	return c.fallbackModels(), nil
}

// modelCatalogue is the catalogue used for model-name mapping and Status.
func (c *Client) modelCatalogue() []core.Model {
	if cached, _ := c.cachedModels(); len(cached) > 0 {
		return cached
	}
	return c.fallbackModels()
}

// modelIDs is the bare list Status reports.
func (c *Client) modelIDs() []string {
	return modelIDsFrom(c.modelCatalogue())
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
		st.Detail = "还没有凭据：在账号页用「浏览器登录」，或从 Qoder CN 客户端导入，" +
			"也可以手动粘贴设备令牌（CLIENT2API_QODER_TOKEN）"
	case usable == 0 && gone > 0:
		st.Ready = false
		st.Detail = "令牌已被厂商拒绝或已过期；请在账号页用「链接重登」重新登录"
	case usable == 0:
		st.Ready = false
		st.Detail = "所有账号都被停用或正在冷却"
	default:
		st.Ready = true
		st.Detail = fmt.Sprintf("%d/%d 个账号可用；本模块提供积分查询、每日签到、活动任务、套餐明细与对话",
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
