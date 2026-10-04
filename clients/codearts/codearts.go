package codearts

import (
	"context"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// codearts.go is the module's entry point: the client type, its constructor,
// and the registration that makes `codearts/<model>` resolvable.
//
// The vendor is 华为 CodeArts (Huawei CodeArts, the model service behind the
// CodeArts IDE).  Everything in this package speaks its protocol directly: a
// temporary AK/SK/security-token triple minted by IAM over OAuth+PKCE, signed
// per request with SDK-HMAC-SHA256, and streamed back as SSE.

// Client is the codearts module.
type Client struct {
	deps core.Deps
	cfg  config
	pool *pool

	// Paths inside Deps.DataDir.  Empty when there is no data directory, in
	// which case nothing is ever written and the module is read-only.
	accountsPath string
	statePath    string
	loginPath    string
	// The catalogue cache is kept apart from the credential store: it is an
	// optimisation, and the reference implementation keeps it under the user's
	// own cache directory.
	modelsPath  string
	benefitPath string

	modelsMu      sync.Mutex
	models        []core.Model
	modelsAt      time.Time
	modelsLoading bool
	modelsRetryAt time.Time
	benefit       []string

	httpOnce sync.Once
	httpLazy *http.Client

	loginMu sync.Mutex
	logins  map[string]*panelLogin
}

// newClient is the core.Factory for this module.
//
// It never fails for a missing or unusable credential — that is a Status()
// condition, not a construction error — and it never fails for a broken config
// either: a config that cannot be decoded is logged and the defaults are used.
// A module that refuses to build takes the whole gateway down with it.
func newClient(deps core.Deps) (core.Client, error) {
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		deps.Log("codearts: invalid config, using defaults: %v", err)
		cfg = config{}
	}
	c := &Client{
		deps:   deps,
		cfg:    cfg,
		pool:   newPool(),
		logins: make(map[string]*panelLogin),
	}
	c.setPaths()
	c.pool.onSave = func(err error) {
		if err != nil {
			c.deps.Log("codearts: persisting credentials: %v", err)
		}
	}
	c.pool.load(configuredAccounts(cfg))
	c.loadModelsCache()
	return c, nil
}

// init registers the module.  clients/all/all.go blank-imports this package,
// and the one line below is what makes the name resolvable.
func init() {
	core.Register("codearts", newClient)
}

// setPaths resolves every file this module may write.
//
// With no data directory the paths stay empty and every writer in the module
// becomes a no-op, which is what lets the module be used read-only.
func (c *Client) setPaths() {
	if c.deps.DataDir == "" {
		return
	}
	if err := core.EnsureDir(c.deps.DataDir); err != nil {
		c.deps.Log("codearts: creating the data directory: %v", err)
		return
	}
	c.accountsPath = filepath.Join(c.deps.DataDir, accountsFile)
	c.statePath = filepath.Join(c.deps.DataDir, stateFile)
	c.loginPath = filepath.Join(c.deps.DataDir, loginFile)
	c.pool.storePath = c.accountsPath
	c.pool.statePath = c.statePath

	// The catalogue cache goes to its own directory when one is configured,
	// and to the data directory otherwise.  It is derived state, so it does
	// not belong beside the credentials.
	dir := c.cfg.cacheDir()
	if dir == "" {
		dir = c.deps.DataDir
	} else if err := core.EnsureDir(dir); err != nil {
		c.deps.Log("codearts: creating the model cache directory: %v", err)
		dir = c.deps.DataDir
	}
	c.modelsPath = filepath.Join(dir, modelsFile)
	c.benefitPath = filepath.Join(dir, benefitFile)
}

// Name is the module's registered name.
func (c *Client) Name() string { return "codearts" }

// Status reports the module's readiness and its per-account state.
//
// It never touches the network: a status call that blocks on an unreachable
// vendor is worse than a status that says the last attempt failed.
func (c *Client) Status(ctx context.Context) core.Status {
	models := c.cachedModels()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)

	st := core.Status{
		Name:      "codearts",
		UpdatedAt: time.Now().UTC(),
		Models:    ids,
		Accounts:  c.pool.snapshot(),
	}
	st.Ready = c.pool.ready()
	if len(c.pool.all()) == 0 {
		st.Detail = "no credential: add an AK/SK pair, sign in, or set " + envAccessKeyID + "/" + envSecretAccessKey
	} else {
		st.Detail = c.pool.summary()
	}
	if !c.modelsFresh(time.Now()) {
		st.Detail += " (model catalogue stale)"
	}
	return st
}

// ---------------------------------------------------------------------------
// optional interfaces
// ---------------------------------------------------------------------------

// The panel and the scheduler discover capabilities by assertion, so every
// interface this module genuinely supports is asserted here at compile time.
// A missing interface is the honest answer for a capability the vendor does
// not have; an assertion here that did not hold would silently hide the
// capability instead.
var (
	_ core.Client              = (*Client)(nil)
	_ core.ModelLimitsProvider = (*Client)(nil)
	_ core.ModelRefresher      = (*Client)(nil)
	_ core.AccountManager      = (*Client)(nil)
	_ core.LoginProvider       = (*Client)(nil)
	_ core.CheckinProvider     = (*Client)(nil)
	_ core.Reviver             = (*Client)(nil)
	_ core.BalanceProvider     = (*Client)(nil)
	_ core.HintProvider        = (*Client)(nil)
)

// ReviveAccount clears an account's runtime penalty state (core.Reviver).
func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	return c.pool.revive(strings.TrimSpace(id))
}

// Hint explains a failure in this module's own words (core.HintProvider).
//
// It only answers for conditions that are specific to CodeArts; anything else
// returns "" so the shared table gets to speak.
func (c *Client) Hint(kind core.FailureKind, message string, hctx core.HintContext) string {
	lower := strings.ToLower(message)
	switch kind {
	case core.FailureAuth:
		return "The CodeArts credential was refused. Its security token is temporary: run a refresh, or sign in again if the refresh token has expired."
	case core.FailureQuota:
		if strings.Contains(lower, "benefit") || strings.Contains(lower, "maas_type") {
			return "This model is not on the free quota. Send it without the benefit header, or pick a model the free-quota list names."
		}
		return "The CodeArts quota for this account is used up. Free-quota models reset daily."
	case core.FailureRateLimited:
		return "CodeArts queues requests when the model is busy; this module already polls the queue and retries. A slower retry usually succeeds."
	}
	if strings.Contains(lower, "reasoning_content") {
		return "CodeArts requires the reasoning_content field on assistant history messages; this module always sends it, so a 400 here means the upstream changed shape."
	}
	if strings.Contains(lower, "context") && strings.Contains(lower, "length") {
		return "The conversation is longer than the model's context window. CodeArts reports this as a 400 rather than trimming, so shorten the history or pick a model with a larger window."
	}
	return c.hintContextNote(hctx)
}

// hintContextNote describes what this module knows about the model a failure
// came from, so a hint can be specific without guessing.
func (c *Client) hintContextNote(hctx core.HintContext) string {
	if hctx.Model != "" && !hctx.ModelInCatalog {
		return "The model " + hctx.Model + " is not in the CodeArts catalogue, so the vendor will refuse it before it reaches a backend."
	}
	return ""
}
