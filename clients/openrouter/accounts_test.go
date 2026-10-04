package openrouter

// Accounts, credential management, error classification and balance.
//
// Everything here runs offline against the fake transport declared in
// openrouter_test.go; no test in this package ever opens a socket.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// --- account fields and listing -------------------------------------------

func TestAccountFieldsListsTheKeyAndLabel(t *testing.T) {
	c := newTestClient(t, "", nil)

	byKey := map[string]core.FieldSpec{}
	for _, s := range c.AccountFields(context.Background()) {
		byKey[s.Key] = s
	}
	key, ok := byKey["api_key"]
	if !ok {
		t.Fatalf("fields = %v, want an api_key field", byKey)
	}
	if key.Type != "password" || !key.Required {
		t.Fatalf("api_key = %+v, want a required password field", key)
	}
	if key.Placeholder == "" {
		t.Fatal("the key field should show the operator the shape of a key")
	}
	if _, ok := byKey["label"]; !ok {
		t.Fatalf("fields = %v, want a label field", byKey)
	}
	if _, ok := byKey["id"]; !ok {
		t.Fatalf("fields = %v, want an id field", byKey)
	}
	// A credential must never be pre-filled into a form.
	for _, s := range c.AccountFields(context.Background()) {
		if strings.Contains(s.Default, "sk-or-") {
			t.Fatalf("field %q has a credential in its default", s.Key)
		}
	}
}

// TestDirectKeyFieldBacksThePanelShortcut pins the capability the panel reads to
// render "use your own API key" beside the browser login.  The key the PKCE
// login mints is scoped to THIS application, so OpenRouter limits it as this
// app; the operator's own key is a different account with its own limits.  The
// panel can only offer the pasted path because the module advertises it, and it
// writes the pasted value into the field the capability names — so a rename
// here that the add form does not follow would break the shortcut silently.
func TestDirectKeyFieldBacksThePanelShortcut(t *testing.T) {
	c := newTestClient(t, "", nil)

	var dk core.DirectKeyProvider = c
	if got := dk.DirectKeyField(context.Background()); got != "api_key" {
		t.Fatalf("DirectKeyField = %q, want api_key", got)
	}
	// The named field has to be one the add form actually collects, or the
	// panel would post a value the module then ignores.
	found := false
	for _, f := range c.AccountFields(context.Background()) {
		if f.Key == "api_key" && f.Type == "password" {
			found = true
		}
	}
	if !found {
		t.Fatal("direct_key names a field the add form does not declare as a password")
	}
	if caps := core.CapabilitiesOf(context.Background(), c); caps.DirectKey != "api_key" {
		t.Fatalf("CapabilitiesOf().DirectKey = %q, want api_key", caps.DirectKey)
	}
}

func TestAccountsListsEveryRecordIncludingDisabled(t *testing.T) {

	c := newTestClient(t, twoKeysCfg(), nil)
	ids := []string{accountIDFor(testKey), accountIDFor(otherKey)}
	if err := c.SetAccountEnabled(context.Background(), ids[1], false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}

	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %d, want every credential listed", len(recs))
	}
	var sawDisabled bool
	for _, rec := range recs {
		if strings.Contains(fmt.Sprintf("%+v", rec), testKey) || strings.Contains(fmt.Sprintf("%+v", rec), otherKey) {
			t.Fatalf("an account record carried the key: %+v", rec)
		}
		if !rec.Enabled {
			sawDisabled = true
		}
	}
	if !sawDisabled {
		t.Fatal("a disabled credential must still be listed, otherwise it cannot be turned back on")
	}
}

func TestAccountsOnAnEmptyPoolIsNotAnError(t *testing.T) {
	c := newTestClient(t, "", nil)
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("an empty pool is a fact, not a failure: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("records = %+v, want none", recs)
	}
}

// --- add / remove / enable -------------------------------------------------

func TestAddAccountStoresAPanelKey(t *testing.T) {
	c := newTestClient(t, "", nil)

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Label:  "my key",
		Fields: map[string]string{"api_key": testKey},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec.ID != accountIDFor(testKey) {
		t.Fatalf("id = %q, want the fingerprint of the key", rec.ID)
	}
	if rec.Label != "my key" || !rec.Enabled || rec.State != "ready" {
		t.Fatalf("record = %+v, want the given label on a ready credential", rec)
	}
	if rec.Fields["source"] != sourcePanel {
		t.Fatalf("source = %v, want %q", rec.Fields["source"], sourcePanel)
	}
	if removable, _ := rec.Fields["removable"].(bool); !removable {
		t.Fatalf("a panel key must be removable: %v", rec.Fields)
	}
	if strings.Contains(fmt.Sprintf("%+v", rec), testKey) {
		t.Fatalf("the account record leaked the key: %+v", rec)
	}
	// It must be durable: the panel adds a credential for the next restart.
	if _, err := os.Stat(c.credentialsPath()); err != nil {
		t.Fatalf("the panel key was not persisted: %v", err)
	}
	if got := loadCredentials(c.credentialsPath()); len(got) != 1 || got[0].APIKey != testKey {
		t.Fatalf("store = %+v, want the panel key", got)
	}
}

func TestAddAccountRejectsEmptyAndWhitespaceKeys(t *testing.T) {
	c := newTestClient(t, "", nil)

	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "   "}}); err == nil {
		t.Fatal("an empty key must be refused")
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "sk-or-v1-abc def"}}); err == nil {
		t.Fatal("a key with whitespace inside it must be refused")
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{}); err == nil {
		t.Fatal("a spec with no fields must be refused")
	}
	if n := len(c.pool.records(c.now())); n != 0 {
		t.Fatalf("records = %d, want a refused key to be stored nowhere", n)
	}
}

func TestAddAccountRefusesToShadowAConfigKey(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)

	_, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": testKey}})
	if err == nil {
		t.Fatal("a key that already comes from the configuration must not be re-added from the panel")
	}
	if !strings.Contains(err.Error(), "configuration") {
		t.Fatalf("err = %v, want it to point at the configuration", err)
	}
}

func TestAddAccountHonoursACustomIDLabelAndDisabledFlag(t *testing.T) {
	c := newTestClient(t, "", nil)
	off := false

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Label:   "team",
		Enabled: &off,
		Fields:  map[string]string{"api_key": otherKey, "id": "team-key"},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec.ID != "team-key" {
		t.Fatalf("id = %q, want the operator's own id from the form", rec.ID)
	}
	if rec.Label != "team" || rec.Enabled {
		t.Fatalf("record = %+v, want the given label and the disabled flag honoured", rec)
	}
	// A disabled credential must not be selectable.
	if _, err := c.pool.acquire(c.now(), c.limit()); !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("acquire = %v, want a disabled-only pool to be unconfigured", err)
	}
}

func TestRemoveAccountRefusesAConfigurationKey(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)

	err := c.RemoveAccount(context.Background(), accountIDFor(testKey))
	if err == nil {
		t.Fatal("a configuration key must not be removable from the panel")
	}
	if !strings.Contains(err.Error(), "clients.openrouter") {
		t.Fatalf("err = %v, want it to name the config key to edit", err)
	}
	if err := c.RemoveAccount(context.Background(), "nope"); err == nil {
		t.Fatal("an unknown id must be an error")
	}
	if n := len(c.pool.records(c.now())); n != 1 {
		t.Fatalf("records = %d, want the refused removal to change nothing", n)
	}
}

func TestRemoveAccountRefusesAnEnvironmentKey(t *testing.T) {
	c := newTestClient(t, "", nil)
	t.Setenv(envAPIKey, testKey)
	c.pool.reload(c.buildCredentials())

	err := c.RemoveAccount(context.Background(), accountIDFor(testKey))
	if err == nil {
		t.Fatal("an environment key must not be removable from the panel")
	}
	if !strings.Contains(err.Error(), envAPIKey) {
		t.Fatalf("err = %v, want it to name %s", err, envAPIKey)
	}
}

func TestRemoveAccountRemovesAPanelKeyFromTheStore(t *testing.T) {
	c := newTestClient(t, "", nil)

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": testKey}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := c.RemoveAccount(context.Background(), rec.ID); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if n := len(c.pool.records(c.now())); n != 0 {
		t.Fatalf("records = %d, want the panel key gone", n)
	}
	if got := loadCredentials(c.credentialsPath()); len(got) != 0 {
		t.Fatalf("store = %+v, want it emptied", got)
	}
}

func TestSetAccountEnabledSurvivesARestart(t *testing.T) {
	c := newTestClient(t, "", nil)

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": testKey}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := c.SetAccountEnabled(context.Background(), rec.ID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	if got := c.mustByID(t, rec.ID); !got.Disabled {
		t.Fatalf("record = %+v, want it disabled", got)
	}

	reopened, err := New(core.Deps{DataDir: c.deps.DataDir, HTTPClient: c.httpClient()})
	if err != nil {
		t.Fatalf("New() after a restart: %v", err)
	}
	next := reopened.(*Client)
	got, ok := next.pool.byID(rec.ID)
	if !ok {
		t.Fatalf("the panel key did not survive the restart: %+v", next.pool.records(testBase))
	}
	if !got.Disabled {
		t.Fatalf("record = %+v, want the disabled state to survive a restart", got)
	}

	if err := c.SetAccountEnabled(context.Background(), "nope", true); err == nil {
		t.Fatal("an unknown id must be an error")
	}
}

// --- test account ---------------------------------------------------------

func TestTestAccountUnknownIDIsAProgrammingError(t *testing.T) {
	c := newTestClient(t, "", nil)
	if _, err := c.TestAccount(context.Background(), "nope"); err == nil {
		t.Fatal("an unknown id must be a Go error, not a result")
	}
}

func TestTestAccountWithoutAKeyIsAResultNotAnError(t *testing.T) {
	c := newTestClient(t, "", nil)
	c.pool.upsert(accountRecord{ID: "blank", Label: "blank", Source: sourcePanel})

	res, err := c.TestAccount(context.Background(), "blank")
	if err != nil {
		t.Fatalf("a keyless account is a result, not a programming mistake: %v", err)
	}
	if res.OK || !strings.Contains(res.Error, "no API key") {
		t.Fatalf("result = %+v, want a refusal naming the missing key", res)
	}
	if res.AccountID != "blank" {
		t.Fatalf("result = %+v, want the account named even on a refusal", res)
	}
}

func TestTestAccountSucceedsAndTakesNoSlot(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: sseBasic} }}
	c := newTestClient(t, cfgWithKey(testKey), up)

	res, err := c.TestAccount(context.Background(), accountIDFor(testKey))
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK || res.Reply != "Hello world" {
		t.Fatalf("result = %+v, want the streamed text", res)
	}
	if res.AccountID != accountIDFor(testKey) {
		t.Fatalf("account = %q, want the tested credential", res.AccountID)
	}
	if up.count() != 1 {
		t.Fatalf("requests = %d, want a single probe", up.count())
	}
	req := up.requestAt(0)
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		t.Fatalf("probe = %s %s, want POST …/chat/completions", req.Method, req.URL.Path)
	}
	// A probe must not hold — or give back — an in-flight slot: the panel can
	// test a key while every slot is busy, and returning a slot the probe never
	// took would let a busy gateway's ceiling drift.  A slot is taken here
	// first, because release() clamps at zero and an idle pool would hide the
	// bug entirely.
	if _, err := c.pool.acquire(c.now(), c.limit()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := c.TestAccount(context.Background(), accountIDFor(testKey)); err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if inFlight, _ := c.pool.stats(c.limit()); inFlight != 1 {
		t.Fatalf("in-flight = %d, want the caller's slot untouched by a probe", inFlight)
	}
}

func TestTestAccountReportsAVendorRefusalAsAResult(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply {
		return reply{status: http.StatusUnauthorized, body: `{"error":{"message":"No cookie auth credentials found","code":401}}`}
	}}
	c := newTestClient(t, cfgWithKey(testKey), up)

	res, err := c.TestAccount(context.Background(), accountIDFor(testKey))
	if err != nil {
		t.Fatalf("a refusal is a result, not an error: %v", err)
	}
	if res.OK || res.Error == "" {
		t.Fatalf("result = %+v, want a refusal the panel can show", res)
	}
	if strings.Contains(res.Error, testKey) {
		t.Fatalf("the refusal leaked the key: %q", res.Error)
	}
	if !strings.Contains(res.Error, "No cookie auth credentials found") {
		t.Fatalf("error = %q, want the vendor's own words", res.Error)
	}
}

// --- refresh account ------------------------------------------------------

func TestRefreshAccountWalksEveryCredential(t *testing.T) {
	up := &fakeUpstream{handle: func(r *http.Request, _ string) reply {
		if strings.HasSuffix(r.URL.Path, "/key") {
			return reply{body: `{"data":{"label":"sk-or-v1-au7...890","limit":100,"limit_remaining":74.5,"usage":25.5}}`}
		}
		return reply{status: http.StatusNotFound, body: `{"error":{"message":"not found","code":404}}`}
	}}
	c := newTestClient(t, twoKeysCfg(), up)

	out, err := c.RefreshAccount(context.Background(), "")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("results = %+v, want one per credential", out)
	}
	for _, res := range out {
		if !res.OK || res.Error != "" {
			t.Fatalf("result = %+v, want every credential refreshed", res)
		}
	}
	if up.count() != 2 {
		t.Fatalf("requests = %d, want one per credential", up.count())
	}
}

func TestRefreshAccountReportsARefusalPerAccount(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply {
		return reply{status: http.StatusUnauthorized, body: `{"error":{"message":"No cookie auth credentials found","code":401}}`}
	}}
	c := newTestClient(t, cfgWithKey(testKey), up)

	out, err := c.RefreshAccount(context.Background(), accountIDFor(testKey))
	if err != nil {
		t.Fatalf("one refused account is not a reason to fail the whole call: %v", err)
	}
	if len(out) != 1 || out[0].OK {
		t.Fatalf("results = %+v, want the refusal reported per account", out)
	}
	if out[0].AccountID != accountIDFor(testKey) || out[0].Error == "" {
		t.Fatalf("result = %+v, want the account named with a reason", out[0])
	}
}

func TestRefreshAccountReportsAKeylessRowWithoutFailing(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply {
		return reply{body: `{"data":{"limit_remaining":1}}`}
	}}
	c := newTestClient(t, cfgWithKey(testKey), up)
	c.pool.upsert(accountRecord{ID: "blank", Label: "blank", Source: sourcePanel})

	out, err := c.RefreshAccount(context.Background(), "")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("results = %+v, want both rows reported", out)
	}
	byID := map[string]core.RefreshResult{}
	for _, res := range out {
		byID[res.AccountID] = res
	}
	if res, ok := byID["blank"]; !ok || res.OK || !strings.Contains(res.Error, "no API key") {
		t.Fatalf("result = %+v, want the keyless row refused with a reason", byID["blank"])
	}
	if res, ok := byID[accountIDFor(testKey)]; !ok || !res.OK {
		t.Fatalf("result = %+v, want the keyed row refreshed", byID[accountIDFor(testKey)])
	}
}

func TestRefreshAccountUnknownIDIsAnError(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	if _, err := c.RefreshAccount(context.Background(), "nope"); err == nil {
		t.Fatal("an unknown id must be a Go error")
	}
}

// --- error classification -------------------------------------------------

func TestClassifyFailurePrecedence(t *testing.T) {
	cases := []struct {
		name   string
		status int
		text   string
		want   failureKind
	}{
		{"a payment demand is a credit verdict", 402, "Payment Required", kindCredit},
		{"a moderated 403 is blocked, not auth", 403, "the prompt was flagged by our moderation system", kindBlocked},
		{"a bare 403 is auth", 403, "forbidden", kindAuth},
		{"401 is auth", 401, "No cookie auth credentials found", kindAuth},
		{"the free-tier daily cap outranks the rate limit", 429, "Rate limit exceeded: free-models-per-day", kindDailyQuota},
		{"a bare 429 is a rate limit", 429, "Too many requests", kindRateLimit},
		{"a credit marker outranks a 400", 400, "Insufficient credits", kindCredit},
		{"an unknown model is not found", 400, "No endpoints found that support tool use", kindNotFound},
		{"500 is the server's fault", 500, "internal error", kindServer},
		{"a bare 400 is a client error", 400, "bad request", kindClient},
		{"a chinese credit marker is understood", 400, "余额不足", kindCredit},
		{"a chinese rate marker is understood", 429, "请求过于频繁", kindRateLimit},
		{"an empty 200 is no verdict at all", 200, "", kindNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFailure(tc.status, 0, tc.text); got != tc.want {
				t.Fatalf("classifyFailure(%d, %q) = %q, want %q", tc.status, tc.text, got, tc.want)
			}
		})
	}
}

func TestCoreKindForMapsEveryVerdict(t *testing.T) {
	cases := map[failureKind]core.FailureKind{
		kindCredit:     core.FailureQuota,
		kindDailyQuota: core.FailureQuota,
		kindRateLimit:  core.FailureRateLimited,
		kindAuth:       core.FailureAuth,
		kindServer:     core.FailureUpstream,
		kindBlocked:    core.FailureContentBlocked,
		kindNotFound:   core.FailureOther,
		kindClient:     core.FailureOther,
		kindNone:       core.FailureOther,
	}
	for k, want := range cases {
		if got := coreKindFor(k); got != want {
			t.Fatalf("coreKindFor(%q) = %q, want %q", k, got, want)
		}
	}
}

func TestFailureKindOfErrorReadsCoreFailures(t *testing.T) {
	cases := []struct {
		kind core.FailureKind
		want failureKind
	}{
		{core.FailureQuota, kindCredit},
		{core.FailureRateLimited, kindRateLimit},
		{core.FailureAuth, kindAuth},
		{core.FailureSessionDead, kindAuth},
		{core.FailureContentBlocked, kindBlocked},
		{core.FailureUpstream, kindServer},
		{core.FailureWAF, kindNone},
	}
	for _, tc := range cases {
		err := core.Fail(clientName, "acct", tc.kind, 0, errors.New("x"))
		if got := failureKindOfError(err); got != tc.want {
			t.Fatalf("failureKindOfError(%q) = %q, want %q", tc.kind, got, tc.want)
		}
		// A wrapped failure must still be recognised (the gateway uses errors.As).
		wrapped := fmt.Errorf("attempt 1: %w", err)
		if got := failureKindOfError(wrapped); got != tc.want {
			t.Fatalf("failureKindOfError(wrapped %q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestClassifyHTTPLeavesANoteButParksNothingForANotFound(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)

	err := c.classifyHTTP("chat", id, http.StatusNotFound, []byte(`{"error":{"message":"No endpoints found for that model","code":404}}`))
	if core.FailureKindOf(err) != core.FailureOther {
		t.Fatalf("kind = %q, want other", core.FailureKindOf(err))
	}
	if core.ErrorAccountID(err) != id || core.ErrorClient(err) != clientName {
		t.Fatalf("failure = %v, want it attributed to %q", err, id)
	}
	rec := c.mustByID(t, id)
	if rec.CooldownUntil != "" {
		t.Fatalf("a model that does not exist parked the key until %s", rec.CooldownUntil)
	}
	if rec.Note == "" {
		t.Fatal("a not-found answer should still leave the panel a note")
	}
}

func TestClassifyHTTPParksForTheConfiguredQuotaCooldown(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)

	err := c.classifyHTTP("chat", id, 402, []byte(`{"error":{"message":"Insufficient credits","code":402}}`))
	if core.FailureKindOf(err) != core.FailureQuota {
		t.Fatalf("kind = %q, want quota", core.FailureKindOf(err))
	}
	rec := c.mustByID(t, id)
	until, perr := time.Parse(time.RFC3339, rec.CooldownUntil)
	if perr != nil {
		t.Fatalf("cooldown = %q, want a timestamp: %v", rec.CooldownUntil, perr)
	}
	if got := until.Sub(c.now()); got != c.cfg.quotaCooldown() {
		t.Fatalf("cooldown = %s, want the configured quota cooldown", got)
	}
	if !strings.Contains(rec.Note, "out of credit") {
		t.Fatalf("note = %q, want it to say why the key is resting", rec.Note)
	}
}

func TestClassifyHTTPMarksARefusedKeyInvalid(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)

	err := c.classifyHTTP("chat", id, http.StatusUnauthorized, []byte(`{"error":{"message":"No cookie auth credentials found","code":401}}`))
	if core.FailureKindOf(err) != core.FailureAuth {
		t.Fatalf("kind = %q, want auth", core.FailureKindOf(err))
	}
	rec := c.mustByID(t, id)
	if !rec.Invalid {
		t.Fatalf("record = %+v, want a refused key marked invalid", rec)
	}
	until, perr := time.Parse(time.RFC3339, rec.CooldownUntil)
	if perr != nil {
		t.Fatalf("cooldown = %q, want a timestamp: %v", rec.CooldownUntil, perr)
	}
	if got := until.Sub(c.now()); got != c.cfg.authCooldown() {
		t.Fatalf("cooldown = %s, want the configured auth cooldown", got)
	}
	if !strings.Contains(rec.Note, "rejected this key") {
		t.Fatalf("note = %q, want the rejection recorded", rec.Note)
	}
}

func TestClassifyHTTPNeverLeaksTheKeyIntoTheMessage(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)

	body := fmt.Sprintf(`{"error":{"message":"invalid key %s","code":401}}`, testKey)
	err := c.classifyHTTP("chat", accountIDFor(testKey), http.StatusUnauthorized, []byte(body))
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("the failure leaked the key: %q", err.Error())
	}
	if rec := c.mustByID(t, accountIDFor(testKey)); strings.Contains(rec.Note, testKey) {
		t.Fatalf("the note leaked the key: %q", rec.Note)
	}
}

func TestClassifyUpstreamWrapsATransportFailure(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)

	err := c.classifyUpstream(id, errors.New("dial tcp 1.2.3.4:443: connect: connection refused"))
	if core.FailureKindOf(err) != core.FailureUpstream {
		t.Fatalf("kind = %q, want upstream", core.FailureKindOf(err))
	}
	if core.ErrorAccountID(err) != id {
		t.Fatalf("account = %q, want %q", core.ErrorAccountID(err), id)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %q, want the transport detail kept", err.Error())
	}
	// A broken socket must not be recorded as "the key was rejected".
	if rec := c.mustByID(t, id); rec.Invalid {
		t.Fatalf("a transport failure marked the key invalid: %+v", rec)
	}
	if err := c.classifyUpstream(id, nil); err != nil {
		t.Fatalf("classifyUpstream(nil) = %v, want nil", err)
	}
}

func TestRetryableChatErrorTable(t *testing.T) {
	cases := []struct {
		status int
		text   string
		want   bool
	}{
		{429, "Too many requests", true},
		{402, "Insufficient credits", true},
		{401, "No cookie auth credentials found", true},
		{500, "internal error", true},
		{400, "bad request", false},
		{404, "No endpoints found", false},
		{403, "the prompt was flagged by our moderation system", false},
	}
	for _, tc := range cases {
		// Every case gets its own client: noteFailure is exercised here.
		c := newTestClient(t, cfgWithKey(testKey), nil)
		body := []byte(fmt.Sprintf(`{"error":{"message":%q,"code":%d}}`, tc.text, tc.status))
		err := c.classifyHTTP("chat", accountIDFor(testKey), tc.status, body)
		if got := retryableChatError(err); got != tc.want {
			t.Fatalf("retryable(%d %q) = %v, want %v", tc.status, tc.text, got, tc.want)
		}
	}
}

func TestErrorTextOfReadsTheOpenRouterEnvelope(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"error":{"message":"No cookie auth credentials found","code":401}}`, "No cookie auth credentials found"},
		{`{"error":{"code":429,"metadata":{"error_type":"rate_limit_exceeded"}}}`, "rate_limit_exceeded"},
		{`{"message":"plain"}`, "plain"},
		{"not json at all", "not json at all"},
	}
	for _, tc := range cases {
		if got := errorTextOf([]byte(tc.body)); got != tc.want {
			t.Fatalf("errorTextOf(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
	// The envelope carries the vendor's own code, which stays out of the
	// gateway's symbolic error code but must be readable for the log.
	if code, msg := parseErrorEnvelope([]byte(`{"error":{"message":"nope","code":4008}}`)); code != 4008 || msg != "nope" {
		t.Fatalf("parseErrorEnvelope = (%d, %q), want (4008, nope)", code, msg)
	}
	if code, msg := parseErrorEnvelope([]byte("garbage")); code != 0 || msg != "" {
		t.Fatalf("parseErrorEnvelope(garbage) = (%d, %q), want (0, \"\")", code, msg)
	}
}

func TestUpstreamErrorMessageShapes(t *testing.T) {
	withStatus := (&upstreamError{Op: "chat", Status: 429, Msg: "slow down"}).Error()
	if withStatus != "openrouter: chat (HTTP 429): slow down" {
		t.Fatalf("got %q", withStatus)
	}
	noStatus := (&upstreamError{Op: "request", Msg: "dial tcp: no route"}).Error()
	if noStatus != "openrouter: request: dial tcp: no route" {
		t.Fatalf("got %q", noStatus)
	}
}

func TestDailyQuotaParkLandsOnMidnightAndIsBounded(t *testing.T) {
	want := time.Date(2025, 3, 5, 0, 0, 0, 0, time.UTC).Sub(testBase)
	if got := dailyQuotaPark(testBase); got != want {
		t.Fatalf("park = %s, want %s (until the next UTC midnight)", got, want)
	}
	// Half a minute before midnight the park must not collapse to nothing.
	late := time.Date(2025, 3, 4, 23, 59, 30, 0, time.UTC)
	if got := dailyQuotaPark(late); got != time.Hour {
		t.Fatalf("park = %s, want the 1h floor", got)
	}
	// And it must never exceed a day, however the clock is skewed.
	if got := dailyQuotaPark(time.Date(2025, 3, 4, 0, 0, 1, 0, time.UTC)); got > 24*time.Hour {
		t.Fatalf("park = %s, want at most 24h", got)
	}
}

func TestScrubForMasksABareKeyTheVendorEchoes(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)

	// core.Redact only knows labelled forms ("api_key=…", "Bearer …"), so a
	// vendor that quotes the key inside a sentence must be masked by hand.
	got := c.scrubFor(id, "the key "+testKey+" is not valid")
	if strings.Contains(got, testKey) {
		t.Fatalf("scrubFor left the key in %q", got)
	}
	if !strings.Contains(got, core.MaskSecret(testKey)) {
		t.Fatalf("scrubFor = %q, want the masked form %q", got, core.MaskSecret(testKey))
	}
	// A labelled key is still caught for an account the pool does not know.
	if got := c.scrubFor("nope", "api_key=sk-or-v1-abcdefghijkl"); strings.Contains(got, "sk-or-v1-abcdefghijkl") {
		t.Fatalf("scrubFor left a labelled key in %q", got)
	}
	// It also truncates: a vendor error is a log line, not a transcript.
	if got := c.scrubFor(id, strings.Repeat("x", 500)); len([]rune(got)) > 301 {
		t.Fatalf("scrubFor kept %d runes, want it truncated", len([]rune(got)))
	}
}

// --- balance --------------------------------------------------------------

func TestAccountBalancePrefersTheCreditsEndpoint(t *testing.T) {
	up := &fakeUpstream{handle: func(r *http.Request, _ string) reply {
		switch {
		case strings.HasSuffix(r.URL.Path, "/credits"):
			return reply{body: `{"data":{"total_credits":100.5,"total_usage":25.75}}`}
		case strings.HasSuffix(r.URL.Path, "/key"):
			return reply{body: `{"data":{"limit_remaining":74.5,"limit":100}}`}
		}
		return reply{status: http.StatusNotFound, body: `{"error":{"message":"not found","code":404}}`}
	}}
	c := newTestClient(t, cfgWithKey(testKey), up)

	bal, err := c.AccountBalance(context.Background(), accountIDFor(testKey), 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 7475 || bal.Total != 10050 || bal.Unit != balanceUnit {
		t.Fatalf("balance = %+v, want 7475 of 10050 %s", bal, balanceUnit)
	}
	if bal.Expiring != 0 || !bal.EarliestAt.IsZero() {
		t.Fatalf("balance = %+v, want no expiring buckets invented", bal)
	}
	if up.count() != 1 || !strings.HasSuffix(up.requestAt(0).URL.Path, "/credits") {
		t.Fatalf("requests = %d (first %v), want only /credits called", up.count(), up.requestAt(0).URL.Path)
	}
}

func TestAccountBalanceFallsBackToTheKeyEndpoint(t *testing.T) {
	// A plain inference key is refused by /credits ("management key required"),
	// so the per-key spending limit is the only number available.
	up := &fakeUpstream{handle: func(r *http.Request, _ string) reply {
		if strings.HasSuffix(r.URL.Path, "/credits") {
			return reply{status: http.StatusForbidden, body: `{"error":{"message":"Only management keys can perform this operation","code":403}}`}
		}
		return reply{body: `{"data":{"label":"sk-or-v1-au7...890","limit":100,"limit_remaining":74.5,"limit_reset":"monthly","usage":25.5}}`}
	}}
	c := newTestClient(t, cfgWithKey(testKey), up)

	bal, err := c.AccountBalance(context.Background(), accountIDFor(testKey), time.Hour)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 7450 || bal.Total != 10000 || bal.Unit != balanceUnit {
		t.Fatalf("balance = %+v, want 7450 of 10000 %s", bal, balanceUnit)
	}
	if up.count() != 2 {
		t.Fatalf("requests = %d, want /credits then /key", up.count())
	}
	// A 403 from /credits says nothing about the key, so nothing may be parked.
	if rec := c.mustByID(t, accountIDFor(testKey)); rec.CooldownUntil != "" {
		t.Fatalf("the fallback parked the key until %s", rec.CooldownUntil)
	}
}

func TestAccountBalanceAcceptsAKeyWithNoSpendingLimit(t *testing.T) {
	up := &fakeUpstream{handle: func(r *http.Request, _ string) reply {
		if strings.HasSuffix(r.URL.Path, "/credits") {
			return reply{status: http.StatusForbidden, body: `{"error":{"code":403,"message":"Only management keys can perform this operation"}}`}
		}
		return reply{body: `{"data":{"limit":null,"limit_remaining":5.25,"usage":0.5,"is_free_tier":true}}`}
	}}
	c := newTestClient(t, cfgWithKey(testKey), up)

	bal, err := c.AccountBalance(context.Background(), accountIDFor(testKey), 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 525 || bal.Total != 0 {
		t.Fatalf("balance = %+v, want 525 with no total", bal)
	}
}

func TestAccountBalanceReportsWhenNeitherEndpointCanAnswer(t *testing.T) {
	up := &fakeUpstream{handle: func(r *http.Request, _ string) reply {
		if strings.HasSuffix(r.URL.Path, "/credits") {
			return reply{status: http.StatusForbidden, body: `{"error":{"code":403,"message":"Only management keys can perform this operation"}}`}
		}
		return reply{body: `{"data":{"label":"x","limit":null,"limit_remaining":null}}`}
	}}
	c := newTestClient(t, cfgWithKey(testKey), up)

	bal, err := c.AccountBalance(context.Background(), accountIDFor(testKey), 0)
	if err == nil {
		t.Fatalf("balance = %+v, want an error rather than a fabricated zero", bal)
	}
	if !strings.Contains(err.Error(), "limit_remaining") {
		t.Fatalf("err = %q, want it to explain the null limit", err)
	}
}

func TestAccountBalanceReportsATransportFailure(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply {
		return reply{err: errors.New("dial tcp 1.2.3.4:443: connect: connection refused")}
	}}
	c := newTestClient(t, cfgWithKey(testKey), up)

	if _, err := c.AccountBalance(context.Background(), accountIDFor(testKey), 0); err == nil {
		t.Fatal("a credit screen with no number is worse than an error")
	}
}

func TestAccountBalanceUnknownOrKeylessAccountIsAnError(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)

	if _, err := c.AccountBalance(context.Background(), "nope", 0); err == nil {
		t.Fatal("an unknown account must be an error")
	}
	c.pool.upsert(accountRecord{ID: "blank", Label: "blank", Source: sourcePanel})
	if _, err := c.AccountBalance(context.Background(), "blank", 0); err == nil {
		t.Fatal("a keyless account must be an error, not a zero balance")
	}
}

func TestParseCreditsRequiresBothNumbers(t *testing.T) {
	bal, ok := parseCredits([]byte(`{"data":{"total_credits":1,"total_usage":2}}`))
	if !ok {
		t.Fatal("parseCredits declined a well-formed payload")
	}
	if bal.Credits != 0 || bal.Total != 100 {
		t.Fatalf("balance = %+v, want the negative remainder clamped to 0", bal)
	}
	if _, ok := parseCredits([]byte(`{"data":{"total_credits":1}}`)); ok {
		t.Fatal("a payload without total_usage must be declined, not guessed")
	}
	if _, ok := parseCredits([]byte(`not json`)); ok {
		t.Fatal("garbage must be declined")
	}
}

func TestParseKeyBalanceRequiresLimitRemaining(t *testing.T) {
	bal, ok := parseKeyBalance([]byte(`{"data":{"limit":100,"limit_remaining":74.5}}`))
	if !ok || bal.Credits != 7450 || bal.Total != 10000 {
		t.Fatalf("balance = (%+v, %v), want 7450 of 10000", bal, ok)
	}
	if _, ok := parseKeyBalance([]byte(`{"data":{"limit":100,"limit_remaining":null}}`)); ok {
		t.Fatal("a null limit_remaining must be declined: there is no number to show")
	}
	if bal, ok := parseKeyBalance([]byte(`{"data":{"limit_remaining":-1}}`)); !ok || bal.Credits != 0 {
		t.Fatalf("balance = (%+v, %v), want a clamp at 0", bal, ok)
	}
	if _, ok := parseKeyBalance([]byte(`not json`)); ok {
		t.Fatal("garbage must be declined")
	}
}

func TestUsdCentsRoundsAndSurvivesGarbage(t *testing.T) {
	if got := usdCents(74.5); got != 7450 {
		t.Fatalf("usdCents(74.5) = %d, want 7450", got)
	}
	if got := usdCents(0); got != 0 {
		t.Fatalf("usdCents(0) = %d, want 0", got)
	}
	// A price of "0.000003" per token is a real value in this catalogue and
	// must not turn into a bogus negative or a huge number.
	if got := usdCents(0.000003); got != 0 {
		t.Fatalf("usdCents(0.000003) = %d, want 0", got)
	}
	if got := usdCents(math.NaN()); got != 0 {
		t.Fatalf("usdCents(NaN) = %d, want 0", got)
	}
	if got := usdCents(math.Inf(1)); got != 0 {
		t.Fatalf("usdCents(+Inf) = %d, want 0", got)
	}
}

func TestBalanceNoteAndKeyLabelHelpers(t *testing.T) {
	if got := balanceNote(core.Balance{Credits: 12, Unit: balanceUnit}); got != "12 USD cents remaining" {
		t.Fatalf("balanceNote = %q", got)
	}
	if got := balanceNote(core.Balance{Credits: 12, Total: 100, Unit: balanceUnit}); got != "12 of 100 USD cents remaining" {
		t.Fatalf("balanceNote = %q", got)
	}
	// The /key label IS a masked key string in the vendor's own example, so a
	// label that looks like a key must never be echoed raw.
	got := keyLabel([]byte(`{"data":{"label":"sk-or-v1-au7...890"}}`))
	if got == "sk-or-v1-au7...890" || !strings.HasPrefix(got, "sk-or-") {
		t.Fatalf("keyLabel = %q, want the masked form", got)
	}
	if got := keyLabel([]byte(`{"data":{"label":"my personal key"}}`)); got != "my personal key" {
		t.Fatalf("keyLabel = %q", got)
	}
	if got := keyLabel([]byte(`{"data":{}}`)); got != "" {
		t.Fatalf("keyLabel = %q, want empty", got)
	}
}

func TestDescribeHTTPFailureShapes(t *testing.T) {
	if got := describeHTTPFailure(0, errors.New("dial tcp: no route to host"), nil); !strings.Contains(got, "no route to host") {
		t.Fatalf("describeHTTPFailure = %q", got)
	}
	if got := describeHTTPFailure(403, nil, []byte(`{"error":{"message":"Only management keys can perform this operation"}}`)); !strings.Contains(got, "Only management keys") {
		t.Fatalf("describeHTTPFailure = %q", got)
	}
	// A status with no body at all must not render as a dangling "HTTP 500: ".
	if got := describeHTTPFailure(500, nil, nil); got != "HTTP 500" {
		t.Fatalf("describeHTTPFailure = %q, want a bare status", got)
	}
	// No status and no error means the request never produced a response.
	if got := describeHTTPFailure(0, nil, nil); got != "no response" {
		t.Fatalf("describeHTTPFailure = %q, want the no-response wording", got)
	}
}
