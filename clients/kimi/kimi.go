// Package kimi implements the "kimi" client module of client2api.
//
// There are two ways to reach the vendor, and this module prefers the second:
//
//  1. the local `kimi` command of Kimi Code CLI, which the MIT-licensed
//     reference it ports (kimi2api, see README.md "Provenance") uses:
//
//     kimi -p <flattened prompt> --output-format stream-json [<permission flag> <mode>] [-m <model>]
//
//     No permission flag is passed by default: prompt mode accepts none.  See
//     defaultPermissionFlag for the flags the real CLI actually rejects.
//
//     One fresh, stateless child process per request; the CLI's NDJSON output
//     is decoded into core.Event values.
//
//  2. plain HTTPS to the coding API the CLI itself talks to
//     (https://api.kimi.com/coding/v1), carrying the OAuth token the CLI
//     persisted.  See direct.go, weblogin.go and models.go.
//
// The HTTP path is the default and the CLI is the fallback, because it needs no
// installed binary and no child process per request.  Either way the module is
// built to degrade honestly: New always succeeds, Models answers from the
// vendor's own list when a credential is available and from the built-in
// catalog otherwise (see models.go), and Chat returns core.ErrNotConfigured
// naming the exact missing prerequisite (binary vs. login).  Nothing is ever
// installed or downloaded by this module.
//
// Requests on the HTTP path carry the identity of the official CLI -- product
// User-Agent, x-msh-* device block, generated-SDK x-stainless-* block -- so the
// vendor sees the client it expects.  See identity.go.
//
// Everything in this package is private to Kimi; no other module may import it.
package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

func init() { core.Register(Name, New) }

// Name is both the registered module name and the routing prefix
// ("kimi/kimi-k2").
const Name = "kimi"

// defaultCLIModel is the model id the CLI uses when -m is omitted.  We omit the
// flag for exactly this id, matching the reference implementation, so that a
// default request is byte-identical to what the CLI's own default produces.
const defaultCLIModel = "kimi"

// ownerKimi is the OwnedBy value reported for every model in the catalog.
const ownerKimi = "moonshot"

// Config is the schema of the "kimi" object inside "clients" in
// configs/client2api.json.  Every field is optional; the zero value is a
// working, conservative configuration.  See README.md for the documented
// schema.
type Config struct {
	// Binary is an explicit path to the kimi executable.  When empty the
	// module searches PATH and then a list of well-known install locations.
	Binary string `json:"binary"`

	// DefaultModel is used when the caller does not name a model.  It is the
	// model id sent to the CLI.
	DefaultModel string `json:"default_model"`

	// PermissionMode is the CLI permission policy.  It defaults to the most
	// restrictive mode we know of, "default"; the reference implementation
	// left the CLI on its own default, which is an auto-approving agent that
	// can run shell commands on this host.  A pointer, so that an explicit
	// "" in the config means "pass no permission flag at all".
	PermissionMode *string `json:"permission_mode"`

	// PermissionFlag is the CLI flag that carries PermissionMode.  It is
	// configurable because the CLI is not installed here and the flag name
	// could not be verified against a real binary.
	PermissionFlag string `json:"permission_flag"`

	// SkillsDir, when set, is passed as --skills-dir.  Empty (the default)
	// passes no such flag, so the user's own skill directory is left alone.
	// The reference hard-coded "empty-skills" here; that silently replaced
	// the user's skills and is deliberately not reproduced.
	SkillsDir string `json:"skills_dir"`

	// ExtraArgs are appended verbatim to the CLI command line.
	ExtraArgs []string `json:"extra_args"`

	// CWD is the working directory for the child process.  Empty means the
	// gateway's own working directory.
	CWD string `json:"cwd"`

	// Env holds extra environment variables for the child process.
	Env map[string]string `json:"env"`

	// MaxConcurrency caps the number of simultaneous child processes.  Zero
	// or negative means the default (2).
	MaxConcurrency int `json:"max_concurrency"`

	// TimeoutSeconds is the wall-clock budget for one request.  Zero or
	// negative means the default (600).
	TimeoutSeconds int `json:"timeout_seconds"`

	// MediaDir is where images extracted from a request are written before
	// the prompt references them with "@/path".  Empty means
	// <DataDir>/media.  It must stay inside DataDir.
	MediaDir string `json:"media_dir"`

	// AllowImageDownload permits fetching http(s) image URLs supplied by the
	// caller.  Off by default: the gateway is reachable by anything on
	// loopback, and fetching an attacker-chosen URL is a server-side request
	// forgery primitive.  data: URLs are always accepted.
	AllowImageDownload bool `json:"allow_image_download"`

	// Models overrides the built-in model catalog.  Empty means the built-in
	// catalog.
	Models []string `json:"models"`

	// CredentialFiles overrides the list of credential files probed for the
	// login check.  Empty means the built-in list.
	CredentialFiles []string `json:"credential_files"`

	// AssumeLoggedIn skips the credential-file login check and lets Chat try
	// the CLI anyway.  Use it when the CLI keeps its token in the OS keyring,
	// which this module cannot read with the standard library.
	AssumeLoggedIn bool `json:"assume_logged_in"`

	// ShowCredentialRows controls whether the account table lists the
	// individual credential files and environment variables the login check
	// found, in addition to the cli-login row that already summarises them.
	// Default true, because knowing which file backed a login is often the
	// point.  Set it to false to keep the account rows only (the panel login
	// and the CLI login) when the per-file rows are just noise: a CLI-owned
	// store then stops looking like a second account.
	ShowCredentialRows *bool `json:"show_credential_rows"`

	// LoginMode selects how the panel's "log in" button works.
	//
	//	"device" (default) - this module runs the RFC 8628 device-code flow
	//	                     against the vendor itself and stores the resulting
	//	                     token in DataDir.  No CLI required.
	//	"cli"              - the original guided flow: the operator runs
	//	                     `kimi login` in a terminal and this module polls
	//	                     for the CLI's own credential to appear.
	LoginMode string `json:"login_mode"`

	// OAuthHost overrides the device-flow issuer.  Empty means the vendor
	// default (https://auth.kimi.com), with KIMI_CODE_OAUTH_HOST and
	// KIMI_OAUTH_HOST consulted first.  Tests point this at a local server.
	OAuthHost string `json:"oauth_host"`

	// OAuthClientID overrides the public device-flow client id.  Empty means
	// the id the CLI ships with.
	OAuthClientID string `json:"oauth_client_id"`

	// PreferHTTP sends chat over the vendor's HTTPS API using the token from a
	// panel login, instead of spawning the CLI.  Defaults to true; set it to
	// false to force every request through the CLI.
	PreferHTTP *bool `json:"prefer_http"`

	// APIBase is the coding API root used by the direct HTTPS path.  Empty
	// means the vendor default for the region.
	APIBase string `json:"api_base"`

	// DeviceID overrides the x-msh-device-id we present.  Empty means: reuse
	// the id the installed CLI persisted, and failing that mint one and keep
	// it.  See identity.go.
	DeviceID string `json:"device_id"`

	// HomeDir overrides where the CLI keeps its state, and with it where we
	// look for a persisted device id.  Empty means ~/.kimi-code.
	HomeDir string `json:"home_dir"`

	// Thinking selects the vendor's thinking extension on a chat request.
	// "enabled" (the default) sends {"type":"enabled","keep":"all"}, which is
	// what the CLI sends; "off" omits the field entirely.
	Thinking string `json:"thinking"`

	// PromptCacheKey overrides the cache-routing key sent as
	// prompt_cache_key.  Empty means one stable "session_<uuid>" per process.
	PromptCacheKey string `json:"prompt_cache_key"`

	// MaxCompletionTokens is the output ceiling sent when the caller's request
	// does not set one.  0 omits the field entirely.  The official CLI always
	// sends a value, derived from the model's context window (the capture shows
	// 262144 for a thinking model), so set this if byte-fidelity with the CLI
	// matters more than letting the vendor choose.
	MaxCompletionTokens int `json:"max_completion_tokens"`

	// TLSProfile and TLSProtocol impersonate the official client's TLS
	// handshake.  Empty keeps the standard library's own handshake, which is
	// what every deployment did before these keys existed.
	TLSProfile  string `json:"tls_profile"`
	TLSProtocol string `json:"tls_protocol"`

	// dataDir is Deps.DataDir, kept for defaulting MediaDir.
	dataDir string `json:"-"`
}

// defaults applied to a Config that came straight from JSON.
const (
	defaultMaxConcurrency = 2
	defaultTimeout        = 600 * time.Second
	// defaultPermissionMode is the most restrictive policy we know how to
	// ask the CLI for.  See README.md, "Permission mode".
	defaultPermissionMode = "default"
	// defaultPermissionFlag is deliberately empty.
	//
	// This was previously "--permission-mode", guessed while no CLI was
	// installed.  The real kimi-code CLI is installed here now (2.1.1) and
	// the guess was wrong in the worst way: prompt mode has no permission
	// flag at all.  "--permission-mode" is rejected with
	// `error: unknown option '--permission-mode'`, and each of the real
	// policy flags is refused in prompt mode too --
	// `error: Cannot combine --prompt with --plan.` (likewise --yolo and
	// --auto).  Passing the guess therefore failed every single CLI request
	// during argument parsing, before the model was ever reached, which is
	// strictly worse than the auto-approval it was meant to prevent.
	//
	// So we invent nothing: prompt mode keeps its own fixed policy.  An
	// operator running a CLI that does take a policy flag sets
	// permission_flag (and permission_mode) explicitly.
	defaultPermissionFlag = ""
)

// parseConfig decodes the module's slice of the main config and applies
// defaults.  A malformed config is a hard error: it is better for the core to
// log "kimi FAILED to load" than to run with silently wrong settings.
func parseConfig(raw json.RawMessage, dataDir string) (Config, error) {
	cfg := Config{dataDir: dataDir}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, err
		}
		cfg.dataDir = dataDir
	}
	if strings.TrimSpace(cfg.DefaultModel) == "" {
		cfg.DefaultModel = defaultCLIModel
	}
	if strings.TrimSpace(cfg.PermissionFlag) == "" {
		cfg.PermissionFlag = defaultPermissionFlag
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = defaultMaxConcurrency
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = int(defaultTimeout / time.Second)
	}
	if strings.TrimSpace(cfg.MediaDir) == "" {
		cfg.MediaDir = filepath.Join(dataDir, "media")
	}
	return cfg, nil
}

// permissionMode returns the mode string to pass, or "" to pass no flag.
func (c Config) permissionMode() string {
	if c.PermissionMode == nil {
		return defaultPermissionMode
	}
	return strings.TrimSpace(*c.PermissionMode)
}

// timeout is the per-request budget.
func (c Config) timeout() time.Duration {
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// loginMode resolves the panel login flow.  An unrecognised value falls back to
// the default rather than failing: a typo in the config should not take the
// login button away.
func (c Config) loginMode() string {
	if strings.EqualFold(strings.TrimSpace(c.LoginMode), loginModeCLI) {
		return loginModeCLI
	}
	return loginModeDevice
}

// preferHTTP reports whether the direct HTTPS path may be used when a token is
// available.  Default true: the panel login is pointless if the module then
// insists on a CLI it no longer needs.
func (c Config) preferHTTP() bool {
	if c.PreferHTTP == nil {
		return true
	}
	return *c.PreferHTTP
}

// showCredentialRows reports whether the account table lists the individual
// credential files and environment variables the login check found.  Default
// true: the evidence rows are how an operator sees which store backed the
// login.  Off keeps only the account rows and lets the cli-login note carry
// the file list, so one account stops looking like several.
func (c Config) showCredentialRows() bool {
	if c.ShowCredentialRows == nil {
		return true
	}
	return *c.ShowCredentialRows
}

// tlsProfile and tlsProtocol name the handshake to imitate.  The empty profile
// is the off switch, so a config that never mentions these keys behaves exactly
// as it did before they existed.
func (c Config) tlsProfile() fingerprint.Profile {
	return fingerprint.Profile(strings.TrimSpace(c.TLSProfile))
}

func (c Config) tlsProtocol() fingerprint.Protocol {
	return fingerprint.Protocol(strings.TrimSpace(c.TLSProtocol))
}

// thinkingOn reports whether chat requests carry the vendor's thinking
// extension.  The CLI always sends it, so that is the default; "off" is the
// escape hatch for a model that rejects it.
func (c Config) thinkingOn() bool {
	return !strings.EqualFold(strings.TrimSpace(c.Thinking), "off")
}

// maxCompletionTokens is the configured output ceiling, or nil to let the
// vendor choose.  A non-positive value is treated as "not set" rather than as a
// request for a zero-token answer.
func (c Config) maxCompletionTokens() *int {
	if c.MaxCompletionTokens <= 0 {
		return nil
	}
	n := c.MaxCompletionTokens
	return &n
}

// Client is the core.Client implementation.  It is safe for concurrent use.
type Client struct {
	deps core.Deps
	cfg  Config
	cat  []core.Model
	run  *runner

	// acct holds the panel-facing account state: the operator's explicit
	// enabled/disabled choices plus any login session in flight.  The durable
	// part lives in <DataDir>/accounts.json (see accounts.go).
	acct accountStore

	// health is what this module has *learned* about each account across runs:
	// cooling/dead, cooldowns, redacted reasons.  It shares the file with acct,
	// but it is a cache rather than a choice -- it can be lost without changing
	// what the operator asked for -- and it is lazy, so a client that never
	// chats never reads it.  See health.go.
	health healthStore

	// identityOnce guards identityVal, the x-msh-* block sent on every vendor
	// call.  It is resolved once because one of its fields costs a filesystem
	// read and another must stay stable across the process's lifetime.
	identityOnce sync.Once
	identityVal  deviceIdentity

	// cacheKeyOnce guards cacheKeyVal, the prompt_cache_key sent on chat
	// requests.  One value per process, minted on first use.  See
	// promptCacheKey.
	cacheKeyOnce sync.Once
	cacheKeyVal  string

	// vendorClient, when set, carries vendor calls over a handshake that
	// imitates the official CLI instead of the standard library's own.  It is
	// nil unless tls_profile names a profile, so an unconfigured module keeps
	// using Deps.HTTPClient and behaves exactly as before.  See New.
	vendorClient *http.Client

	mu      sync.Mutex
	lastErr string

	// modelsMu guards the upstream model-list cache.  Models() is called on
	// every panel refresh, so the vendor is asked only when the cache is empty
	// or older than modelsCacheTTL; keeping the last good answer here is what
	// makes a failed refresh unable to blank the picker.  See models.go.
	modelsMu    sync.Mutex
	modelsList  []core.Model
	modelsAt    time.Time
	modelsTried time.Time

	// affinity remembers which account a conversation was served by, so a
	// follow-up turn does not silently move to the other serving path (the
	// panel login and the CLI are two different credentials).  It is created
	// unconditionally -- unlike the pool modules, kimi always has at most two
	// serving identities, and an empty table is exactly the old behaviour.  See
	// affinity.go.
	affinity *core.Affinity
}

// New builds the module.  It never fails because the CLI or the credentials are
// missing: that is what Status() and the ErrNotConfigured paths are for.
func New(deps core.Deps) (core.Client, error) {
	cfg, err := parseConfig(deps.Config, deps.DataDir)
	if err != nil {
		return nil, fmt.Errorf("kimi: bad config: %w", err)
	}
	c := &Client{
		deps: deps,
		cfg:  cfg,
		cat:  catalog(cfg),
	}
	c.run = newRunner(c)
	c.affinity = core.NewAffinity(0)
	c.affinity.StartGC()
	if fp := cfg.tlsProfile(); fp != fingerprint.ProfileNone {
		fhc, err := fingerprint.New(fingerprint.Options{
			Profile:  fp,
			Protocol: cfg.tlsProtocol(),
			Proxy:    deps.Proxy,
			Logf:     deps.Logf,
		})
		if err != nil {
			// A profile the operator asked for but that we cannot build is
			// worth shouting about, but not worth failing startup over: the
			// module still works, it just looks like Go on the wire.
			c.deps.Log("kimi: tls fingerprint %q ignored: %v", fp, err)
		} else {
			c.vendorClient = fhc
			c.deps.Log("kimi: tls handshake imitates the %s client", fp)
		}
	}
	c.deps.Log("config: binary=%q model=%q permission_mode=%q skills_dir=%q max_concurrency=%d timeout=%s assume_logged_in=%v",
		cfg.Binary, cfg.DefaultModel, cfg.permissionMode(), cfg.SkillsDir, cfg.MaxConcurrency, cfg.timeout(), cfg.AssumeLoggedIn)
	return c, nil
}

// Name implements core.Client.
func (c *Client) Name() string { return Name }

// Models implements core.Client.  It always answers, and it stays cheap: the
// panel calls it on every refresh, so it reads a TTL cache and only reaches the
// vendor when that cache is empty or stale.  With no credential, or when the
// vendor cannot be reached, it answers from the baseline catalog (Config.Models
// when the operator set one, otherwise builtinCatalog()).  See models.go.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	return c.listModels(ctx)
}

// Chat implements core.Client.
//
// The returned stream owns a child process and any media files it created;
// callers must Close it.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if req == nil {
		return nil, fmt.Errorf("kimi: nil request: %w", core.ErrUnsupported)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// A token obtained by the panel's own login makes the CLI unnecessary: the
	// vendor serves the same models over HTTPS (see direct.go).  Prefer that
	// path, but keep the CLI as a fallback so a stale token cannot take a
	// machine that has a working CLI offline.
	var directErr error
	// webNote remembers why the panel login was passed over.  The CLI path
	// below can only report what it knows, so when it finds no credential
	// evidence it used to answer "run `kimi login`" even though the operator's
	// login was the panel one and the actual fault was that this module had
	// parked it.  Naming the real reason is the difference between an
	// actionable error and a wrong instruction.
	var webNote, webFix string

	// A conversation the panel pinned to one account must be served by that
	// account.  Falling through to the other path would answer from a
	// credential the operator did not choose, which is the whole point of the
	// pin -- so it is applied as a path decision below, not as a hint.
	key := conversationKey(req)
	pinned, hasPin := c.affinityPin(key, req.Model)

	useWeb := c.cfg.preferHTTP()
	if hasPin && pinned != webLoginID {
		// Pinned to the CLI side: the panel login must not be consulted at all.
		useWeb = false
	}
	if useWeb && c.accountExplicitlyDisabled(webLoginID) {
		webNote = "it is disabled in the panel"
		webFix = "re-enable it in the panel"
		useWeb = false
	}
	if useWeb && !c.selectable(webLoginID) {
		// The panel login is parked by what this module learned earlier (a
		// refused credential, a call that failed).  The CLI is a genuine
		// fallback, so this is not an error for the request; the reason is on
		// the account rows and in the log.
		webNote = c.healthNote(webLoginID)
		webFix = "sign in again from the panel, which replaces the token and revives the account"
		c.deps.Log("kimi: the panel login is not usable right now (%s), falling back to the CLI", webNote)
		useWeb = false
	}
	if useWeb {
		// freshToken renews a grant that is at or near its stated expiry.  This is
		// what keeps a panel login alive: the access token the device flow issues
		// is short lived, so without renewal the account dies within the hour and
		// the operator is told to sign in again for no reason.
		tok, ok, renewErr := c.freshToken(ctx)
		if renewErr != nil {
			c.noteError(renewErr)
			// A refused grant is the one vendor answer this module can classify
			// as dead; a transport failure is remembered as a reason and retried
			// rather than parking the account (see classifyLocally).
			c.noteFailure(webLoginID, renewErr, "", 0)
			c.deps.Log("kimi: the stored credential could not be renewed: %v", renewErr)
		}
		if ok && !tok.expired() {
			// Take the web account's slot before the network call.  A ceiling
			// refusal is transient, so an unpinned request falls back to the
			// CLI (another account) instead of being reported as a failure.
			if slotErr := req.AcquireAccountSlot(webLoginID); slotErr != nil {
				if hasPin && pinned == webLoginID {
					return nil, slotErr
				}
				c.deps.Log("kimi: web account %s is at its concurrency ceiling; using the CLI", webLoginID)
			} else {
				st, derr := c.directChat(ctx, req, tok)
				if derr == nil {
					c.clearError()
					c.markUsed(webLoginID)
					c.bindConversation(key, webLoginID)
					// Name the credential for the gateway's usage ledger and
					// console row.  kimi has two accounts rather than a pool, and
					// this is the branch that served the turn.
					core.NoteServedBy(req, webLoginID)
					return st, nil
				}
				directErr = derr
				c.noteError(derr)
				c.noteFailure(webLoginID, derr, "", 0)
				c.deps.Log("kimi: the direct HTTPS path failed, falling back to the CLI: %v", derr)
				if hasPin && pinned == webLoginID {
					// The operator asked for this account specifically.  Serving the
					// turn from the CLI would answer the question with the wrong
					// credential, so the failure is the honest answer; the pin is
					// dropped by the table on the next request anyway, because
					// noteFailure above just parked this account.
					return nil, fmt.Errorf("the conversation is pinned to account %s and this request will not be retried on the CLI: %w",
						webLoginID, derr)
				}
			}
		}
	}

	bin, binAccount, err := c.run.binaryPathFrom()
	if err != nil {
		c.noteError(err)
		// "There is no kimi CLI here" is local, cheap to recheck and fixable in
		// a minute by an install, so it cools instead of dying: the account
		// becomes selectable again when the cooldown passes, which is long
		// enough to stop the panel re-deciding this on every refresh.
		c.noteFailure(cliLoginID, err, causeCLINotFound, cooldownCLIMissing)
		if directErr != nil {
			return nil, fmt.Errorf(
				"%v. The kimi CLI was not found on PATH or in any known install location either, so there is no fallback "+
					"(set clients.kimi.binary, or sign in again from the panel): %w",
				directErr, core.ErrNotConfigured)
		}
		return nil, fmt.Errorf(
			"kimi CLI not found on PATH or in any known install location (set clients.kimi.binary to point at it). "+
				"Install it with: irm https://code.kimi.com/kimi-code/install.ps1 | iex  then run `kimi login`: %w",
			core.ErrNotConfigured)
	}

	creds := c.credentials()
	// An account the operator explicitly disabled in the panel is a decision,
	// not a guess: refuse before the "is anything logged in" check so the
	// operator sees their own choice reflected back.
	if derr := c.loginDisabledError(creds); derr != nil {
		c.noteError(derr)
		return nil, fmt.Errorf("%s: %w", derr.Error(), core.ErrNotConfigured)
	}
	if !creds.usable() && !c.cfg.AssumeLoggedIn {
		c.noteError(errNotLoggedIn)
		// Locally provable, and time cannot fix it: with no credential evidence
		// anywhere this module can look, the login is dead until positive
		// evidence appears (see reviveOnEvidence below) or the operator acts.
		c.markDead(cliLoginID, causeNoCredential,
			"no login credential was found where this module looks; the CLI may still hold a keyring token, which the standard library cannot read")
		// When the panel login is stored but was passed over, that is the
		// reason this request failed.  Telling the operator to run `kimi login`
		// would send them to fix the wrong thing, so name the login the panel
		// actually owns and the action that revives it.
		if webNote != "" {
			return nil, fmt.Errorf(
				"the panel login (account %s) is stored but not usable right now (%s), and the kimi CLI at %s has no login credentials either "+
					"(looked for: %s): %s: %w",
				webLoginID, webNote, bin, strings.Join(creds.searched, ", "), webFix, core.ErrNotConfigured)
		}
		return nil, fmt.Errorf(
			"kimi CLI found at %s but no login credentials were found; run `kimi login`. "+
				"Looked for: %s (and the environment variables %s): %w",
			bin, strings.Join(creds.searched, ", "), strings.Join(credentialEnvVars, ", "), core.ErrNotConfigured)
	}
	// Credential evidence appearing is the second thing that may revive a dead
	// account, and the one a restart cannot see: `kimi login` in a terminal
	// happens outside the panel, so the panel has no action to hang a revival
	// on.  Disproving the very condition that killed the account is new
	// information, so the record goes.
	c.reviveOnEvidence(cliLoginID, causeNoCredential, creds.usable() || c.cfg.AssumeLoggedIn)

	// Everything above refused for reasons that are true *now*.  What is left is
	// the memory of a failure a stat cannot see -- a run that failed a minute
	// ago -- which is exactly what this gate is for: a binary this module has
	// just watched fail must not be picked again on the next request.
	//
	// It is keyed on the account the resolved binary actually belongs to, not on
	// cli-login: a path found on PATH is cli-login's business, while a binary the
	// operator bound in the panel answers for itself.  Keying it on cli-login
	// would let one cooling search take down a binding that is fine.
	if !c.selectable(binAccount) {
		reason := fmt.Sprintf("kimi: account %q is not usable right now: %s", binAccount, c.healthNote(binAccount))
		c.noteError(errors.New(reason))
		return nil, fmt.Errorf("%s: %w", reason, core.ErrNotConfigured)
	}
	if hasPin && pinned != binAccount {
		// The conversation is pinned to a CLI-side account other than the one
		// the runner resolved -- a different imported binding, or cli-login
		// against a binding.  Naming the mismatch is the honest answer; serving
		// anyway would test an account the operator did not pick.
		reason := fmt.Sprintf("kimi: the conversation is pinned to account %q, but the kimi CLI resolved to %q", pinned, binAccount)
		c.noteError(errors.New(reason))
		return nil, fmt.Errorf("%s: %w", reason, core.ErrNotConfigured)
	}
	// Recorded at selection time rather than on success: two concurrent first
	// turns of the same conversation must agree on one account instead of racing
	// to write two.
	c.bindConversation(key, binAccount)
	// Same reasoning for the gateway's usage ledger: the CLI account is settled
	// here, and every remaining failure path either names it itself or is
	// correctly attributed to it.
	// Take the CLI account's slot before spawning the process.  Spawning is
	// the expensive operation the ceiling exists to bound.
	if err := req.AcquireAccountSlot(binAccount); err != nil {
		c.noteError(err)
		return nil, err
	}
	core.NoteServedBy(req, binAccount)

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = c.cfg.DefaultModel
	}

	prompt, media, err := c.buildPrompt(ctx, req)
	if err != nil {
		c.noteError(err)
		return nil, err
	}

	args := c.cliArgs(prompt, model)

	// Backpressure: never spawn an unbounded number of agent processes.
	select {
	case c.run.sem <- struct{}{}:
	case <-ctx.Done():
		media.cleanup()
		return nil, fmt.Errorf("kimi: waiting for a free slot (max_concurrency=%d): %w", c.cfg.MaxConcurrency, ctx.Err())
	}

	runCtx, cancel := context.WithTimeout(ctx, c.cfg.timeout())
	st, err := startStream(runCtx, cancel, c, binAccount, bin, args, media)
	if err != nil {
		<-c.run.sem
		cancel()
		media.cleanup()
		c.noteError(err)
		return nil, err
	}
	return st, nil
}

// Status implements core.Client.  It is deliberately cheap: two small file
// probes at most, both cached, and never a network call.
func (c *Client) Status(ctx context.Context) core.Status {
	st := core.Status{
		Name:      Name,
		UpdatedAt: time.Now(),
		Models:    modelIDs(c.cat),
	}

	bin, binErr := c.run.binaryPath()
	creds := c.credentials()
	// A token this module obtained itself is an account in its own right, and
	// its presence is what makes the CLI optional.
	tok, hasToken := c.loadToken()
	// A grant whose short-lived access token has lapsed is still live while it
	// carries a refresh token: the next request renews it.  Reporting that as
	// "not signed in" would send the operator through a browser login for
	// nothing.
	tokenRenewable := hasToken && strings.TrimSpace(tok.RefreshToken) != ""
	tokenLive := hasToken && (!tok.expired() || tokenRenewable) && !c.accountExplicitlyDisabled(webLoginID)

	switch {
	case tokenLive:
		st.Ready = true
		st.Detail = fmt.Sprintf("signed in from the panel via %s; requests go straight to %s over HTTPS, so the kimi CLI is not required",
			tok.OAuthHost, c.apiBase(tok))
		if tok.expired() {
			st.Detail += "; the access token is past its stated expiry and is renewed from the saved refresh token on the next request"
		}
		if binErr == nil {
			st.Detail += fmt.Sprintf(" (CLI also available at %s)", bin)
		}
	case c.cfg.loginMode() == loginModeDevice && !c.cfg.AssumeLoggedIn && (binErr != nil || !creds.usable()):
		// The browser flow talks to the issuer directly, so it is this module's
		// preferred path and the CLI is at most a footnote.  Opening with an
		// install nag here sends the operator off to install a program the
		// module does not need, so say what to click first and keep the CLI
		// state as the secondary fact it is.
		st.Ready = false
		if binErr != nil {
			// No install command here on purpose: in device mode the browser is
			// the supported path, and printing `irm … | iex` is what made the
			// panel look like it was demanding a CLI.  Operators who really want
			// the CLI set `login_mode: "cli"` and get the hint below.
			st.Detail = fmt.Sprintf("not signed in yet; click the panel's login button to sign in with a browser at %s. "+
				"The kimi CLI was not found on PATH or in any known install location, but it is not required",
				c.oauthHost())
		} else {
			st.Detail = fmt.Sprintf("not signed in yet; click the panel's login button to sign in with a browser at %s. "+
				"The kimi CLI at %s is not logged in (no credential file among %s) and is not required",
				c.oauthHost(), bin, strings.Join(creds.searched, ", "))
		}
	case binErr != nil:
		st.Ready = false
		st.Detail = "kimi CLI not found on PATH or in any known install location; " + installHint
	case !creds.usable() && !c.cfg.AssumeLoggedIn:
		st.Ready = false
		st.Detail = fmt.Sprintf("kimi CLI found at %s but not logged in (no credential file among %s); run `kimi login`",
			bin, strings.Join(creds.searched, ", "))
	default:
		st.Ready = true
		st.Detail = fmt.Sprintf("kimi CLI at %s; model %s; permission mode %s; max_concurrency %d",
			bin, c.cfg.DefaultModel, orNone(c.cfg.permissionMode()), c.cfg.MaxConcurrency)
	}

	if last := c.lastError(); last != "" {
		st.Detail += "; last error: " + last
	}
	st.Accounts = creds.accounts(bin, binErr, c.cfg)
	if acct, ok := c.tokenStatus(); ok {
		st.Accounts = append([]core.AccountStatus{acct}, st.Accounts...)
	}
	// Fold in the operator's explicit panel choices, then make Ready agree with
	// them: a login the panel shows as disabled must not look usable here.
	if note := c.applyAccountOverrides(st.Accounts); note != "" {
		st.Detail += "; account state unreadable: " + note
	}
	if derr := c.loginDisabledError(creds); derr != nil {
		st.Ready = false
		st.Detail = derr.Error()
		if binErr == nil {
			st.Detail += " (CLI at " + bin + ")"
		}
		if last := c.lastError(); last != "" {
			st.Detail += "; last error: " + last
		}
	}
	// The rows above carry the health memory, so Ready has to agree with them.
	// Reporting a client as ready while every request answers 503 is the one
	// lie the panel cannot afford: the operator reads the badge, believes it,
	// and stops looking.  This is the same rule the pool-backed modules follow
	// (workbuddy checks pool.Ready(), trae checks pool.Ready() && usableNow(),
	// tabbit checks usable > 0); this module has no pool, so the rows are the
	// authority.
	if st.Ready && len(st.Accounts) > 0 {
		usable, note := 0, ""
		for _, row := range st.Accounts {
			if accountRowUsable(row) {
				usable++
				continue
			}
			if note == "" {
				note = strings.TrimSpace(row.Note)
			}
		}
		if usable == 0 {
			st.Ready = false
			st.Detail = "no account can serve a request right now"
			if note != "" {
				st.Detail += ": " + note
			}
		}
	}
	return st
}

// accountRowUsable reports whether a Status row can still serve a request.  An
// unknown state counts as usable on purpose: the health memory is what lowers a
// row, and a module that cannot read its own state file must not invent an
// outage out of a field it failed to fill in.
func accountRowUsable(row core.AccountStatus) bool {
	if !row.Enabled {
		return false
	}
	switch row.State {
	case "invalid", "cooling", "disabled", "dead":
		return false
	}
	return true
}

func (c *Client) noteError(err error) {
	if err == nil {
		return
	}
	msg := redactSecrets(err.Error())
	if len(msg) > 240 {
		msg = msg[:240] + "…"
	}
	c.mu.Lock()
	c.lastErr = msg
	c.mu.Unlock()
}

func (c *Client) clearError() {
	c.mu.Lock()
	c.lastErr = ""
	c.mu.Unlock()
}

func (c *Client) lastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}
