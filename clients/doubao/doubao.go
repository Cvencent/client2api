// Package doubao implements the "doubao" client: ByteDance's mainland Doubao
// (Doubao) consumer assistant, spoken to over the www.doubao.com web API.
//
// # WHAT THIS MODULE DOES
//
// It owns the cookie account pool and serves chat through the same route the
// desktop client's own web app uses:
//
//   - GET  /alice/user/get_account_info  -- proves a cookie session is live, and
//     is what the web login polls; and
//   - POST /samantha/chat/completion     -- the streamed completion, whose
//     event-stream frames this module translates to core.Events.
//
// The account form is a right-to-use access request.  The vendor requires a
// Doubao account for the API to respond; the model runtime is reached through
// the same cookie session as all other requests.
//
// The credential is the browser's own session cookie jar.  This module never
// mints one: the sign-in belongs to the vendor, and the honest remedy when the
// vendor stops accepting a cookie is to sign in again through the web login
// (login.go), which drives a real browser and reads the HttpOnly cookies back.
package doubao

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"client2api/internal/core"
)

// Client implements core.Client for doubao, plus the optional interfaces named
// in the assertion block below.
type Client struct {
	deps core.Deps
	cfg  config
	up   *upstream

	store *store

	// Model catalogue cache.  Models is called on every panel refresh and must
	// stay cheap; RefreshModels is the explicit synchronous re-read.
	modelMu  sync.Mutex
	catalog  []core.Model
	catalogAt time.Time

	// inFlight is the number of chat streams currently open.  Chat feeds it and
	// PoolStats reports it, so the panel shows real process state.
	inFlight atomic.Int64

	// affinity pins a conversation to the account that first served it, so the
	// second turn does not walk to the least-recently-used credential.
	affinity *core.Affinity

	// loginMu guards the in-flight web-login sessions.  A login that survived a
	// restart is a login nobody can reason about, so sessions are memory-only.
	loginMu sync.Mutex
	logins  map[string]*loginSession

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
		// A typo in the config must never take the module off the panel.
		deps.Log("doubao: invalid config, using defaults: %v", err)
		cfg = config{}
	}
	applyEnv(&cfg)

	if deps.DataDir != "" {
		if err := core.EnsureDir(deps.DataDir); err != nil {
			deps.Log("doubao: creating data dir: %v", err)
		}
	}

	c := &Client{deps: deps, cfg: cfg}
	c.affinity = core.NewAffinity(0)
	c.affinity.StartGC()
	c.setPaths()

	st, err := loadStore(c.accountsPath, c.statePath)
	if err != nil {
		// A corrupt store is reported and the module carries on empty: the
		// operator can re-import, which beats refusing to start.
		deps.Log("doubao: %v", err)
	}
	c.store = st
	c.store.applyConfig(configuredAccounts(cfg))

	c.up = newUpstream(cfg, deps.HTTPClient, deps.Logf)
	return c, nil
}

func init() { core.Register("doubao", New) }

// setPaths resolves the module's files inside Deps.DataDir.
func (c *Client) setPaths() {
	if c.deps.DataDir == "" {
		return
	}
	c.accountsPath = filepath.Join(c.deps.DataDir, accountsFile)
	c.statePath = filepath.Join(c.deps.DataDir, stateFile)
}

// Name is both the registry key and the routing prefix ("doubao/<model>").
func (c *Client) Name() string { return "doubao" }

// The panel and the scheduler discover capabilities by assertion, so every
// interface this module genuinely supports is asserted here at compile time.
var (
	_ core.Client              = (*Client)(nil)
	_ core.AccountManager      = (*Client)(nil)
	_ core.LoginProvider       = (*Client)(nil)
	_ core.BalanceProvider     = (*Client)(nil)
	_ core.ModelRefresher      = (*Client)(nil)
	_ core.HealthProvider      = (*Client)(nil)
	_ core.PoolStatsReporter   = (*Client)(nil)
	_ core.Reviver             = (*Client)(nil)
	_ core.ConversationBinder  = (*Client)(nil)
)

// Models answers from the cached catalogue and falls back to the built-in list
// on a cold cache.  Registry.Resolve calls Models for bare model names, so this
// path never performs a network round trip.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	c.modelMu.Lock()
	cached := append([]core.Model(nil), c.catalog...)
	at := c.catalogAt
	c.modelMu.Unlock()

	if len(cached) > 0 {
		if time.Since(at) >= c.cfg.modelsTTL() {
			c.refreshModelsAsync()
		}
		return cached, nil
	}
	return c.builtinModels(), nil
}

// modelCatalogue is the catalogue used for Status.
func (c *Client) modelCatalogue() []core.Model {
	c.modelMu.Lock()
	cached := append([]core.Model(nil), c.catalog...)
	c.modelMu.Unlock()
	if len(cached) > 0 {
		return cached
	}
	return c.builtinModels()
}

// modelIDs is the bare list Status reports.
func (c *Client) modelIDs() []string {
	models := c.modelCatalogue()
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// Status is deliberately cheap: no network calls and one mutex-guarded
// snapshot.  The panel calls it every ten seconds.
func (c *Client) Status(ctx context.Context) core.Status {
	now := time.Now().UTC()
	st := core.Status{
		Name:      "doubao",
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
		if accounts[i].State == stateInvalid {
			gone++
		}
	}
	st.Accounts = records

	switch {
	case len(accounts) == 0:
		st.Ready = false
		st.Detail = "Doubao: no credential yet -- use the web login in the account page, " +
			"or paste a cookie from a signed-in browser session"
	case usable == 0 && gone > 0:
		st.Ready = false
		st.Detail = "Doubao rejected the stored cookie sessions; sign in again with the web login to refresh them"
	case usable == 0:
		st.Ready = false
		st.Detail = "every Doubao account is disabled or cooling down"
	default:
		st.Ready = true
		st.Detail = fmt.Sprintf("%d/%d Doubao account(s) usable; this module serves chat and reports credits",
			usable, len(accounts))
	}

	if msg, at := c.lastFailure(); msg != "" && !st.Ready {
		st.Detail += "; last error " + msg + " (" + humanAge(time.Since(at)) + " ago)"
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
	c.lastErr = core.Redact(err.Error())
	c.lastErrAt = time.Now()
}

func (c *Client) lastFailure() (string, time.Time) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.lastErr, c.lastErrAt
}

// humanAge renders a duration the way the other modules do.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// redactErr is a local alias so the module's message paths stay uniform.
func redactErr(err error) string {
	if err == nil {
		return ""
	}
	return core.Redact(err.Error())
}

var _ = http.StatusOK
