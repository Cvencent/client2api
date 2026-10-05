package core

import (
	"context"
	"strings"
)

// ---------------------------------------------------------------------------
// Optional capabilities.
//
// Client is the ONLY interface a module must implement.  Everything in this
// file is optional: a module implements the capabilities its vendor actually
// supports and ignores the rest.  The panel discovers them with a type
// assertion on the registered Client value, so:
//
//   - the panel never names a concrete client, and
//   - adding a capability to core cannot break a module that lacks it.
//
// That keeps the isolation rule intact: one module can grow full account
// management while another stays a read-only status card.
// ---------------------------------------------------------------------------

// FieldSpec describes one input on a module's "add account" form.  The panel
// renders these verbatim, so a module owns its own credential schema without
// the panel knowing anything about it.
type FieldSpec struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"` // text|password|textarea|number|bool|select
	Required    bool     `json:"required,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Help        string   `json:"help,omitempty"`
	Options     []string `json:"options,omitempty"`
	// Default pre-fills the input.  Never put a real credential here.
	Default string `json:"default,omitempty"`
}

// AccountRecord is one credential as the panel sees it.  It must never carry a
// secret: the panel is served over plain HTTP on loopback and its output is
// cached by browsers.
type AccountRecord struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	// OperatorNote is the panel's own label for this account: the phone
	// number or e-mail the operator recorded for it.  Modules must leave it
	// empty -- the panel fills it in from the gateway config before serving
	// the account list -- and it exists so a row, and the re-login it offers,
	// can name the identity that credential signs in as.
	OperatorNote string `json:"operator_note,omitempty"`
	Enabled      bool   `json:"enabled"`
	State        string `json:"state"` // ready|cooling|exhausted|invalid|unknown
	// Priority is the operator's routing priority for this account.  Lower
	// numbers are tried first; zero is the default.
	Priority  int            `json:"priority,omitempty"`
	ExpiresAt string         `json:"expires_at,omitempty"`
	Note      string         `json:"note,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"` // non-secret extras
	// Identity is the vendor's own id for the account this credential belongs
	// to, when the module can determine it.  It is what lets the panel show one
	// account once instead of once per credential: two records that report the
	// same non-empty Identity are the same account reached through different
	// channels (a plan JWT and a coding-plan API key for one Zhipu user, say),
	// and the panel groups them under a single row.
	//
	// It is deliberately not a secret — it is the account id the vendor itself
	// prints in its tokens and in its credential-store key names — but it is
	// redacted with everything else, because a module that ever puts token
	// material here must not be able to leak it through this field.
	//
	// Empty means "this module cannot say", and such a record is its own
	// account: guessing an identity would merge unrelated logins, which is
	// worse than showing them apart.
	Identity string `json:"identity,omitempty"`
}

// AccountSpec is a credential being added by hand.  Only the keys the module
// declared in AccountFields are meaningful; unknown keys are the module's
// business to ignore.  Values are always strings because that is what an HTML
// form produces — a module that wants a number or a bool parses it itself.
type AccountSpec struct {
	ID      string            `json:"id,omitempty"`
	Label   string            `json:"label,omitempty"`
	Enabled *bool             `json:"enabled,omitempty"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// TestResult is the outcome of a live probe against the vendor's upstream.
type TestResult struct {
	OK        bool   `json:"ok"`
	AccountID string `json:"account_id,omitempty"`
	Model     string `json:"model,omitempty"`
	Reply     string `json:"reply,omitempty"`
	Error     string `json:"error,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms,omitempty"`
}

// RefreshResult is the outcome of renewing one credential.
type RefreshResult struct {
	AccountID string `json:"account_id"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

// DiscoveredCredential is one credential found on this machine that could be
// imported.  Path is shown to the operator, so it must be a real path the
// module is willing to have displayed.
type DiscoveredCredential struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"` // e.g. "storage.json", "auth.dat", "cli-login"
	Label      string `json:"label,omitempty"`
	Note       string `json:"note,omitempty"`
	Importable bool   `json:"importable"`
	Imported   bool   `json:"imported,omitempty"`
}

// LoginState is the progress of an interactive login (device code, OAuth
// redirect, or a CLI the operator has to drive themselves).
type LoginState struct {
	SessionID string `json:"session_id,omitempty"`
	State     string `json:"state"` // pending|success|failed|cancelled
	URL       string `json:"url,omitempty"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	// Realm is the upstream family this login targets, echoed back so the
	// panel can label the result.  Empty for a single-realm module.
	Realm string `json:"realm,omitempty"`
}

// LoginRealm is one upstream family a module's interactive login can target.
// A vendor that serves a mainland service and a separate international one
// reports both, and the operator picks before the flow starts -- adding an
// account to the wrong realm produces a credential that can never serve the
// models the other realm lists.
type LoginRealm struct {
	Code string `json:"code"`           // passed back to StartLoginRealm: "cn" | "global"
	Name string `json:"name"`           // operator-facing label
	Help string `json:"help,omitempty"` // one line shown under the picker
}

// Login states.
const (
	LoginPending   = "pending"
	LoginSuccess   = "success"
	LoginFailed    = "failed"
	LoginCancelled = "cancelled"
)

// CheckinAction is one daily-reward action a module can perform.  A vendor may
// have more than one — a CN realm "check-in" that mints credits, an intl realm
// "daily activity" that needs a chat turn — so the panel renders the list
// instead of assuming a single button.
type CheckinAction struct {
	ID    string `json:"id"`    // passed back to Checkin
	Label string `json:"label"` // operator-facing button text
	Help  string `json:"help,omitempty"`
}

// CheckinResult is the outcome of one check-in attempt.
//
// A refusal by the upstream — already checked in today, wrong realm, expired
// credential — is a RESULT, not an error: OK is false and Error says why.
type CheckinResult struct {
	OK        bool           `json:"ok"`
	AccountID string         `json:"account_id,omitempty"`
	Action    string         `json:"action,omitempty"`
	Code      int            `json:"code,omitempty"`
	Message   string         `json:"message,omitempty"`
	Error     string         `json:"error,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	ElapsedMS int64          `json:"elapsed_ms,omitempty"`
	At        string         `json:"at,omitempty"` // RFC3339, when it finished
}

// Capabilities tells the panel what a module can be asked to do.  It is built
// by CapabilitiesOf, so the panel never inspects concrete types.
type Capabilities struct {
	Manage  bool            `json:"manage"`         // AccountManager
	Import  bool            `json:"import"`         // CredentialImporter
	Login   bool            `json:"login"`          // LoginProvider
	Checkin bool            `json:"checkin"`        // CheckinProvider
	Refresh bool            `json:"refresh_models"` // ModelRefresher
	Tasks   bool            `json:"tasks"`          // TaskProvider
	Fields  []FieldSpec     `json:"fields,omitempty"`
	Actions []CheckinAction `json:"actions,omitempty"`

	// CheckinReady is the narrow half of Checkin: at least one action is on
	// offer for an account row right now.  Every other flag in this block is a
	// plain interface assertion that no account can change, and Checkin is too
	// — but CheckinActions is allowed to narrow itself to the accounts that
	// exist at this instant (codearts reports nothing while it holds no
	// credential, minimaxcode nothing while the credential is expired), so
	// Checkin alone cannot tell the panel "this module does check-in" apart
	// from "this module could check in if you gave it an account".  Only the
	// per-account button, which is rendered on a row that exists, may read
	// CheckinReady; the client-level capability matrix reads Checkin, exactly
	// as it reads Tasks and the rest of the opt-in block below.
	CheckinReady bool `json:"checkin_ready"`
	// Bundle says the module implements BundleImporter, so the panel may offer
	// an "upload an export file" control.  It is separate from Import on
	// purpose: Import walks files the module found on this machine itself,
	// Bundle accepts a document the operator supplies, and a module that can do
	// one cannot necessarily do the other.
	Bundle bool `json:"import_bundle"`

	// Realms is non-empty only for a RealmLoginProvider: the panel renders a
	// realm picker from it, and hides the picker entirely when it is empty.
	Realms []LoginRealm `json:"realms,omitempty"`

	// KeyPages is the provider->console map a KeyPageProvider returns.  The
	// panel shows the matching link as the operator moves through the add
	// form's provider options, so a multi-source module can point at each
	// vendor's key page without hard-coding the table in JavaScript.
	KeyPages map[string]string `json:"key_pages,omitempty"`

	// DirectKey is the AccountFields key a module accepts the operator's OWN
	// credential in (DirectKeyProvider).  It is the panel's licence to render a
	// "paste your own key" control beside the browser login, and it names the
	// field the pasted value belongs in.  Empty means the module does not offer
	// one: its login is the only way in, and a second path would just 404.
	DirectKey string `json:"direct_key,omitempty"`

	// The block below is the "opt-in" half of the contract: the shared
	// machinery in this package (error classification, failover pacing,
	// degraded retry, batch planning, health reporting, live settings, account
	// revival) only starts working for a module that implements the matching
	// interface.  Nothing forces a module to, and a module that does not must
	// keep working exactly as before -- so the panel has to be able to show
	// which modules actually opted in.  Without that, an unimplemented
	// capability looks identical to a broken one.
	Degrade  bool `json:"degrade"`  // Degrader: retry once with the degraded prompt
	Batches  bool `json:"batches"`  // BatchPlanner: can feed the scheduler
	Health   bool `json:"health"`   // HealthProvider: reports servability
	Live     bool `json:"live"`     // LiveReloader: accepts live settings
	Revive   bool `json:"revive"`   // Reviver: an operator can clear its penalties
	Balance  bool `json:"balance"`  // BalanceProvider: can report credits left
	Packages bool `json:"packages"` // PackageProvider: can break credits into tranches
	Vouchers bool `json:"vouchers"` // VoucherProvider: holds redemption codes
	// Conversations says the module implements ConversationBinder, so an
	// operator can pin one conversation key to one account and thereby test
	// that account through the ordinary chat route.  Without it the panel must
	// not offer a per-account conversation test at all: the request would be
	// served by whichever account the pool happened to pick, and a green
	// result would then be a lie about the account the operator selected.
	Conversations bool `json:"conversations"` // ConversationBinder
	// The task verbs.  Tasks says the module has a board at all; these three
	// say which of the reference's per-task buttons it can actually serve, so
	// the frontend can hide the rest rather than offering a 501.
	TaskAccept bool `json:"task_accept"` // TaskAccepter and/or BulkTaskAccepter
	TaskClaim  bool `json:"task_claim"`  // TaskClaimer
	TaskAuto   bool `json:"task_auto"`   // TaskAutoRunner

	// Captcha says the module implements CaptchaProvider: at least one of its
	// actions wants a token minted in a real browser, and the panel should run
	// the vendor's SDK before calling it.  It is a property of the MODULE, not
	// of an action -- whether a given action is gated is answered per call by
	// CaptchaScene.Required.
	Captcha bool `json:"captcha"` // CaptchaProvider

	// SMS says the module implements SMSProvider: it can rent a phone number
	// from a one-time-SMS platform and read the code that arrives, so an
	// account can be added without the operator owning a phone.  The panel
	// renders the "接码" controls only for a module that opted in.
	SMS bool `json:"sms"` // SMSProvider

	// AutoLogin says the module implements AutoLoginProvider: it can run the
	// whole vendor login itself in a browser it drives -- rent a number, fill
	// the vendor's form, read the code the platform received, submit -- so the
	// panel offers a one-click "自动添加" instead of the operator-driven link
	// flow.  It is separate from SMS on purpose: SMSProvider alone only means
	// "this module can rent a number", which the panel drives by hand.
	AutoLogin bool `json:"auto_login"` // AutoLoginProvider
}

// AccountManager lets a module expose its credential store to the panel.
//
// Implement it only if a human can meaningfully list, add, test or remove this
// vendor's credentials.  A module whose credentials come solely from another
// program's login (and cannot be created by hand) should not implement it.
type AccountManager interface {
	Client
	// AccountFields describes the add form.  An empty slice means the module
	// can be listed and tested but nothing can be typed in — the panel then
	// hides the form and keeps the table.
	AccountFields(ctx context.Context) []FieldSpec
	// Accounts lists what the module currently holds.  It must not fail just
	// because the pool is empty: return an empty slice.
	Accounts(ctx context.Context) ([]AccountRecord, error)
	// AddAccount stores one credential.  The module validates and may return
	// an error naming the offending field.
	AddAccount(ctx context.Context, spec AccountSpec) (AccountRecord, error)
	// RemoveAccount deletes one credential by ID.
	RemoveAccount(ctx context.Context, id string) error
	// SetAccountEnabled toggles one credential without deleting it.
	SetAccountEnabled(ctx context.Context, id string, enabled bool) error
	// TestAccount probes one credential against the live upstream.
	TestAccount(ctx context.Context, id string) (TestResult, error)
	// RefreshAccount renews one credential, or every credential when id is
	// empty.  It reports per-account outcomes and only fails wholesale when
	// the request itself is malformed.
	RefreshAccount(ctx context.Context, id string) ([]RefreshResult, error)
}

// CredentialImporter lets a module pull credentials out of the vendor's own
// desktop application.  Discover must be read-only; Import is the only call
// allowed to write.
type CredentialImporter interface {
	Client
	// Discover lists what it found.  Read-only.
	Discover(ctx context.Context) ([]DiscoveredCredential, error)
	// Import imports the named paths, or everything Discover marked
	// importable when all is true.  An empty paths with all false is a no-op.
	Import(ctx context.Context, paths []string, all bool) ([]AccountRecord, error)
}

// BundleImportReport is the outcome of one ImportBundle call.
//
// The per-entry failures live in Errors rather than in the returned error: one
// unusable entry in a hundred must not throw away the ninety-nine that worked,
// and the operator needs to see which one was refused and why.  A non-nil error
// means the document itself could not be read at all.
type BundleImportReport struct {
	Total    int      `json:"total"`
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
}

// BundleImporter lets a module ingest an export document that only that module
// understands — the credential dump of some third-party companion tool, say.
//
// The shared layer deliberately does not parse the document: the panel hands
// the bytes over and reports the module's verdict.  That keeps a vendor-specific
// export format out of internal/panel, where it would become one more thing
// every other module had to be compared against.
type BundleImporter interface {
	Client
	// ImportBundle ingests one document.  name is the operator's file name and
	// is for labelling only; data is the raw upload.  Implementations must
	// bound how much of data they trust and must never echo a secret back in
	// the report.
	ImportBundle(ctx context.Context, name string, data []byte) (BundleImportReport, error)
}

// LoginProvider lets a module run an interactive login from the panel.  The
// flow is: StartLogin, then poll PollLogin until State is no longer pending,
// or CancelLogin to give up.
type LoginProvider interface {
	Client
	StartLogin(ctx context.Context) (LoginState, error)
	PollLogin(ctx context.Context, sessionID string) (LoginState, error)
	CancelLogin(ctx context.Context, sessionID string) error
}

// DirectKeyProvider marks a module whose browser login mints a credential the
// VENDOR scopes to this application -- so the vendor's limits for this
// application are the ones that apply -- but which also accepts the operator's
// own credential for the same upstream.
//
// OpenRouter is the reason this exists: the key the PKCE login issues is
// rate-limited as this app, while a key from the operator's own account page
// is limited by that account and spends that account's free quota.  The two
// are different accounts, so the panel offers the pasted path only for a
// module that opts in here -- a module whose login is the only way in keeps
// its single-button form.
type DirectKeyProvider interface {
	Client
	// DirectKeyField names the AccountFields key the pasted credential belongs
	// in.  An empty string means the same as not implementing this interface.
	DirectKeyField(ctx context.Context) string
}

// KeyPageProvider lets a module that stores several upstreams at once (one
// account per provider, as openai-compat does) tell the panel where the
// operator goes to create a key for a given provider.  The panel renders the
// link when the add form's provider selection changes, so the operator is one
// click from the vendor's console instead of guessing the URL.
//
// This is deliberately separate from AccountFields: a module with a single
// provider still gets its form built from the field specs alone, while a
// multi-provider module can answer the same question for every id it lists.
// An empty or missing map leaves the panel with no link, exactly as before.
type KeyPageProvider interface {
	Client
	// KeyPageURLs maps a provider id (the value the operator picks in the
	// account form) to the page where that provider's key is created.
	KeyPageURLs(ctx context.Context) map[string]string
}

// RealmLoginProvider is a LoginProvider whose flow branches on which upstream
// realm the account is being added to.  It is a separate interface on purpose:
// the shared machinery is opt-in, so a module with a single realm keeps
// implementing LoginProvider alone and the panel simply never renders a picker.
//
// A module that implements it must keep StartLogin working -- it is defined as
// "the module's configured default realm", so an operator who never touches the
// picker sees exactly the behaviour they had before.
type RealmLoginProvider interface {
	LoginProvider
	// LoginRealms lists the realms this module can add an account to.  An
	// empty slice means "there is no choice to make".
	LoginRealms(ctx context.Context) []LoginRealm
	// StartLoginRealm starts a login pinned to one realm.  An empty realm
	// asks for the module's configured default.
	StartLoginRealm(ctx context.Context, realm string) (LoginState, error)
}

// CheckinProvider lets a module run the vendor's daily reward action for one
// stored account.  It is optional and orthogonal to AccountManager: a module
// can offer check-in without letting the operator add credentials.
type CheckinProvider interface {
	Client
	// CheckinActions lists what this client can do.  It is safe to call with
	// no account configured, and it must never depend on which request arrived
	// — but it may narrow itself to the accounts that exist at this instant,
	// and six of the seven modules do: codearts answers nothing while it holds
	// no credential, minimaxcode nothing while the credential is expired.  That
	// is why Capabilities carries two bits.  Checkin answers the client-level
	// question ("does this module do check-in at all?") and is what the
	// capability matrix reads; CheckinReady answers "is there a button to offer
	// on an account row right now?".  An empty slice therefore means no
	// per-account button — never "this module cannot check in".
	CheckinActions(ctx context.Context) []CheckinAction
	// Checkin performs one action for one account.  An empty action selects
	// the client's default.  Return a non-nil error only when the attempt
	// could not be made at all (unknown account id, no store configured) —
	// an upstream refusal is a CheckinResult with OK false.
	Checkin(ctx context.Context, id, action string) (CheckinResult, error)
}

// ModelRefresher lets a module re-read its model catalogue from the vendor on
// demand.  Models is called on every panel refresh, so it must stay cheap: a
// module that answers it from a cache and only reaches upstream on a timer
// implements this to give the panel's "refresh" button real work to do.
//
// Implementing it is a promise that the answer can change without a restart.
// A module whose catalogue is baked into the binary must NOT implement it —
// the panel would then offer a button that provably does nothing.
type ModelRefresher interface {
	Client
	// RefreshModels bypasses any cache and asks the vendor again.  A failure
	// must not empty the catalogue: return the last good list alongside the
	// error so one flaky refresh cannot blank the model picker.
	RefreshModels(ctx context.Context) ([]Model, error)
}

// CapabilitiesOf reports what one module can do.  It is the only place that
// turns concrete types into something the panel can serialise.
func CapabilitiesOf(ctx context.Context, c Client) Capabilities {
	var caps Capabilities
	if am, ok := c.(AccountManager); ok {
		caps.Manage = true
		caps.Fields = am.AccountFields(ctx)
	}
	if _, ok := c.(CredentialImporter); ok {
		caps.Import = true
	}
	if _, ok := c.(BundleImporter); ok {
		caps.Bundle = true
	}
	if _, ok := c.(LoginProvider); ok {
		caps.Login = true
	}
	if dk, ok := c.(DirectKeyProvider); ok {
		caps.DirectKey = dk.DirectKeyField(ctx)
	}
	if kp, ok := c.(KeyPageProvider); ok {
		caps.KeyPages = kp.KeyPageURLs(ctx)
	}
	if rl, ok := c.(RealmLoginProvider); ok {
		caps.Realms = rl.LoginRealms(ctx)
	}
	if cp, ok := c.(CheckinProvider); ok {
		caps.Checkin = true
		caps.Actions = cp.CheckinActions(ctx)
		caps.CheckinReady = len(caps.Actions) > 0
	}
	if _, ok := c.(ModelRefresher); ok {
		caps.Refresh = true
	}
	if _, ok := c.(CaptchaProvider); ok {
		caps.Captcha = true
	}
	if _, ok := c.(SMSProvider); ok {
		caps.SMS = true
	}
	if _, ok := c.(AutoLoginProvider); ok {
		caps.AutoLogin = true
	}
	if _, ok := c.(TaskProvider); ok {
		caps.Tasks = true
	}
	if _, ok := c.(Degrader); ok {
		caps.Degrade = true
	}
	if _, ok := c.(BatchPlanner); ok {
		caps.Batches = true
	}
	if _, ok := c.(HealthProvider); ok {
		caps.Health = true
	}
	if _, ok := c.(LiveReloader); ok {
		caps.Live = true
	}
	if _, ok := c.(Reviver); ok {
		caps.Revive = true
	}
	if _, ok := c.(BalanceProvider); ok {
		caps.Balance = true
	}
	if _, ok := c.(PackageProvider); ok {
		caps.Packages = true
	}
	if _, ok := c.(VoucherProvider); ok {
		caps.Vouchers = true
	}
	if _, ok := c.(ConversationBinder); ok {
		caps.Conversations = true
	}
	// The task verbs.  accept_all implies accept: a module that can take on
	// everything at once can certainly take on a named subset, and the panel
	// renders one button for the family either way.
	if _, ok := c.(TaskAccepter); ok {
		caps.TaskAccept = true
	}
	if _, ok := c.(BulkTaskAccepter); ok {
		caps.TaskAccept = true
	}
	if _, ok := c.(TaskClaimer); ok {
		caps.TaskClaim = true
	}
	if _, ok := c.(TaskAutoRunner); ok {
		caps.TaskAuto = true
	}
	return caps
}

// AsAccountManager narrows a registered client.  The panel uses it so a module
// without the capability answers 501 instead of panicking.
func AsAccountManager(c Client) (AccountManager, bool) {
	am, ok := c.(AccountManager)
	return am, ok
}

// AsCredentialImporter narrows a registered client.
func AsCredentialImporter(c Client) (CredentialImporter, bool) {
	ci, ok := c.(CredentialImporter)
	return ci, ok
}

// AsBundleImporter narrows a registered client to the operator-supplied export
// path.  Kept separate from AsCredentialImporter so a module that only walks
// local files is never asked to parse a document it does not own.
func AsBundleImporter(c Client) (BundleImporter, bool) {
	bi, ok := c.(BundleImporter)
	return bi, ok
}

// AsLoginProvider narrows a registered client.
func AsLoginProvider(c Client) (LoginProvider, bool) {
	lp, ok := c.(LoginProvider)
	return lp, ok
}

// AsRealmLoginProvider narrows a registered client to the realm-aware login
// flow.  The panel keeps using AsLoginProvider for everything else, so a module
// that only implements LoginProvider is driven exactly as before.
func AsRealmLoginProvider(c Client) (RealmLoginProvider, bool) {
	rp, ok := c.(RealmLoginProvider)
	return rp, ok
}

// AsCheckinProvider narrows a registered client.
// AsSMSProvider narrows a registered client to the one-time-SMS platform
// capability.  The panel answers 501 for a module that never opted in.
func AsSMSProvider(c Client) (SMSProvider, bool) {
	sp, ok := c.(SMSProvider)
	return sp, ok
}

// AsAutoLoginProvider narrows a registered client to the browser-driven login
// capability.  The panel answers 501 for a module that never opted in.
func AsAutoLoginProvider(c Client) (AutoLoginProvider, bool) {
	ap, ok := c.(AutoLoginProvider)
	return ap, ok
}

func AsCheckinProvider(c Client) (CheckinProvider, bool) {
	cp, ok := c.(CheckinProvider)
	return cp, ok
}

// AsModelRefresher narrows a registered client.
func AsModelRefresher(c Client) (ModelRefresher, bool) {
	mr, ok := c.(ModelRefresher)
	return mr, ok
}

// Field reads one string out of an AccountSpec, trimming surrounding space.
func (s AccountSpec) Field(key string) string {
	if s.Fields == nil {
		return ""
	}
	return strings.TrimSpace(s.Fields[key])
}

// FieldOr reads one string out of an AccountSpec, falling back to def.
func (s AccountSpec) FieldOr(key, def string) string {
	if v := s.Field(key); v != "" {
		return v
	}
	return def
}

// EnabledOr reports the requested enabled state, defaulting to true so that a
// credential added without an explicit choice is active.
func (s AccountSpec) EnabledOr(def bool) bool {
	if s.Enabled != nil {
		return *s.Enabled
	}
	return def
}
