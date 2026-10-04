package codearts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// codearts_test.go covers the client itself: construction, Status, the model
// catalogue surface, and the exact set of optional interfaces this module
// claims — and, just as importantly, the ones it does not.

// setModelsForTest seeds the catalogue cache exactly the way a successful
// refresh would, so the cache paths can be exercised without a vendor.
func (c *Client) setModelsForTest(models []core.Model, at time.Time) {
	c.modelsMu.Lock()
	c.models = append([]core.Model(nil), models...)
	c.modelsAt = at
	c.modelsRetryAt = time.Time{}
	c.modelsMu.Unlock()
}

// The name is what `codearts/<model>` resolves through, so it is part of the
// module's public contract.
func TestNameIsCodearts(t *testing.T) {
	c := newTestClient(t, `{}`)
	if got := c.Name(); got != "codearts" {
		t.Fatalf("Name() = %q, want codearts", got)
	}
}

// The registry is what makes the module reachable at all.
func TestFactoryIsRegistered(t *testing.T) {
	found := false
	for _, name := range core.Registered() {
		if name == "codearts" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("core.Registered() = %v, want it to contain codearts", core.Registered())
	}
	built, err := core.Build("codearts", core.Deps{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("core.Build(codearts): %v", err)
	}
	if built.Name() != "codearts" {
		t.Fatalf("the registered factory built %q", built.Name())
	}
}

// A config that cannot be decoded must not take the module down: the whole
// gateway imports every module, so a panic here is an outage.
func TestNewClientWithABrokenConfigUsesDefaults(t *testing.T) {
	var logged []string
	deps := core.Deps{
		DataDir: t.TempDir(),
		Config:  json.RawMessage(`{"max_attempts": "not a number"}`),
		Logf:    func(format string, args ...any) { logged = append(logged, format) },
	}
	got, err := newClient(deps)
	if err != nil {
		t.Fatalf("newClient with a broken config: %v", err)
	}
	c := got.(*Client)
	// The defaults, not zeroes.
	if c.cfg.maxAttempts() != defaultMaxAttempts {
		t.Errorf("maxAttempts() = %d, want the default %d", c.cfg.maxAttempts(), defaultMaxAttempts)
	}
	if len(logged) == 0 {
		t.Error("a broken config was accepted silently; the operator should be told")
	}
}

// With no data directory the module is read-only rather than broken, which is
// what lets it be constructed in a context that has no writable state.
func TestNewClientWithoutADataDirIsReadOnly(t *testing.T) {
	got, err := newClient(core.Deps{Config: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("newClient without a data dir: %v", err)
	}
	c := got.(*Client)
	if c.accountsPath != "" || c.statePath != "" || c.modelsPath != "" {
		t.Fatalf("paths = %q/%q/%q, want all empty", c.accountsPath, c.statePath, c.modelsPath)
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"access_key_id": "AK", "secret_access_key": "SK"},
	}); err == nil {
		t.Error("AddAccount without a data dir reported success; nothing was stored")
	}
}

func TestStatusWithoutAnAccount(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	st := c.Status(context.Background())
	if st.Name != "codearts" {
		t.Errorf("Status().Name = %q, want codearts", st.Name)
	}
	if st.Ready {
		t.Error("Status().Ready = true with no credential")
	}
	if !strings.Contains(st.Detail, "no credential") {
		t.Errorf("Detail = %q, want it to say there is no credential", st.Detail)
	}
	if len(st.Accounts) != 0 {
		t.Errorf("Accounts = %v, want none", st.Accounts)
	}
	if st.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not set")
	}
}

func TestStatusWithAnAccount(t *testing.T) {
	c := newTestClient(t, `{
		"base_url":"http://127.0.0.1:1",
		"access_key_id":"AKIDTESTACCOUNT","secret_access_key":"SK","security_token":"TOK"}`)
	st := c.Status(context.Background())
	if !st.Ready {
		t.Fatalf("Status().Ready = false, want true; detail = %q", st.Detail)
	}
	if len(st.Accounts) != 1 {
		t.Fatalf("Accounts = %v, want exactly one", st.Accounts)
	}
	a := st.Accounts[0]
	if a.Identity != "AKIDTESTACCOUNT" {
		t.Errorf("Identity = %q, want the access key id", a.Identity)
	}
	if !a.Enabled {
		t.Error("the account is disabled")
	}
	if a.Extra["origin"] != originConfig {
		t.Errorf("Extra[origin] = %v, want %q", a.Extra["origin"], originConfig)
	}
	if a.Extra["refreshable"] != false {
		t.Errorf("Extra[refreshable] = %v, want false: this credential has no refresh token", a.Extra["refreshable"])
	}
	if a.State == "" {
		t.Error("the account has no state, so the panel cannot render it")
	}

	// The record view carries the credential facts the status view does not.
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("Accounts = %v, want exactly one", recs)
	}
	if recs[0].Fields["has_token"] != true {
		t.Errorf("Fields[has_token] = %v, want true", recs[0].Fields["has_token"])
	}
	if recs[0].Identity != "AKIDTESTACCOUNT" {
		t.Errorf("Identity = %q, want the access key id", recs[0].Identity)
	}
	// The panel must never be handed the secret itself.
	if s, _ := recs[0].Fields["access_key_id"].(string); s == "AKIDTESTACCOUNT" {
		t.Error("the account record carries the unmasked access key id")
	}
	for k, v := range recs[0].Fields {
		if s, ok := v.(string); ok && strings.Contains(s, "SK") && k != "access_key_id" {
			t.Errorf("Fields[%s] = %q looks like it carries the secret", k, s)
		}
	}

	// Status must never touch the network, so it cannot be slow.
	if time.Since(st.UpdatedAt) > time.Second {
		t.Error("Status took over a second; it must not call the vendor")
	}
}

// Status is the one call the panel makes on a timer, so it must not block on an
// unreachable vendor.
func TestStatusDoesNotCallTheVendor(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, `{
		"base_url":`+jsonString(srv.URL)+`,
		"chat_url":`+jsonString(srv.URL+"/chat")+`,
		"access_key_id":"AK","secret_access_key":"SK","security_token":"TOK"}`)
	_ = c.Status(context.Background())
	if hits != 0 {
		t.Fatalf("Status made %d upstream calls, want 0", hits)
	}
}

// waitForModelRefresh blocks until the background catalogue refresh kicked off
// by Models has finished.  RefreshModels persists the catalogue before the
// goroutine clears modelsLoading, so once that flag is down the disk write is
// already done — which is the point: otherwise the goroutine outlives the test
// and writes into t.TempDir after cleanup has run, failing the package with
// "the directory is not empty".
func waitForModelRefresh(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.modelsMu.Lock()
		loading := c.modelsLoading
		c.modelsMu.Unlock()
		if !loading {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the background catalogue refresh did not finish in time")
}

// Models answers from the cache so a request that names a model never waits on
// the catalogue.  An unreachable vendor must therefore be invisible here.
func TestModelsAnswersFromTheCacheWithoutWaiting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately slow: Models must not wait for it.
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"builtinModels":[]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, `{
		"base_url":`+jsonString(srv.URL)+`,
		"models_url":`+jsonString(srv.URL+"/models")+`,
		"gateway_url":`+jsonString(srv.URL+"/gw")+`,
		"access_key_id":"AK","secret_access_key":"SK","security_token":"TOK"}`)

	started := time.Now()
	models, err := c.Models(context.Background())
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("Models took %v, want it to answer from the cache", elapsed)
	}
	// With no cache yet it serves the built-in fallback rather than nothing.
	if len(models) == 0 {
		t.Fatal("Models returned nothing before the first refresh; the fallback list should have been served")
	}
	// Models kicked off a refresh in the background; let it finish before the
	// test — and t.TempDir's cleanup — goes away.
	waitForModelRefresh(t, c)
}

// The per-model output budget is the interface the gateway fills max_tokens
// from, so an absent model must answer ok=false rather than a guess.
func TestModelMaxOutputTokens(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	// Seed the cache directly: the fallback catalogue is what Models would
	// serve, and it carries the built-in budgets.
	c.setModelsForTest(c.fallbackCatalogue(), time.Now())

	id := c.cachedModels()[0].ID
	got, ok := c.ModelMaxOutputTokens(context.Background(), id)
	if !ok {
		t.Fatalf("ModelMaxOutputTokens(%q) = ok=false, want the built-in budget", id)
	}
	if got <= 0 || got > maxOutputTokens {
		t.Fatalf("ModelMaxOutputTokens(%q) = %d, want a sane positive budget", id, got)
	}
	// A model nobody knows must not be guessed at.
	if _, ok := c.ModelMaxOutputTokens(context.Background(), "no-such-model"); ok {
		t.Error("ModelMaxOutputTokens guessed a budget for an unknown model")
	}
	if _, ok := c.ModelMaxOutputTokens(context.Background(), ""); ok {
		t.Error("ModelMaxOutputTokens answered for an empty model name")
	}
}

// The panel hides a capability that is not implemented, so the set has to be
// exactly right — both ways.
func TestCapabilitiesReportTheImplementedSet(t *testing.T) {
	c := newTestClient(t, `{
		"base_url":"http://127.0.0.1:1",
		"access_key_id":"AKIDTESTACCOUNT","secret_access_key":"SK","security_token":"TOK"}`)
	caps := core.CapabilitiesOf(context.Background(), c)

	for name, got := range map[string]bool{
		"Manage":   caps.Manage,
		"Login":    caps.Login,
		"Checkin":  caps.Checkin,
		"Refresh":  caps.Refresh,
		"Revive":   caps.Revive,
		"Balance":  caps.Balance,
		"Import":   caps.Import,
		"Bundle":   caps.Bundle,
		"Tasks":    caps.Tasks,
		"Batches":  caps.Batches,
		"Health":   caps.Health,
		"Live":     caps.Live,
		"Packages": caps.Packages,
		"Vouchers": caps.Vouchers,
		"Captcha":  caps.Captcha,
	} {
		want := name == "Manage" || name == "Login" || name == "Checkin" ||
			name == "Refresh" || name == "Revive" || name == "Balance"
		if got != want {
			t.Errorf("Capabilities.%s = %v, want %v", name, got, want)
		}
	}
	if len(caps.Fields) == 0 {
		t.Error("Manage is advertised but the credential form is empty")
	}
	if len(caps.Actions) != 1 {
		t.Errorf("Actions = %v, want exactly the daily sign-in", caps.Actions)
	}
	if len(caps.Realms) != 0 {
		t.Errorf("Realms = %v, want none: CodeArts has one portal, not several", caps.Realms)
	}
}

// The check-in button must disappear rather than fail when nothing is
// configured -- but the module must still report that it HAS check-in, or the
// panel's client-level capability matrix tells the operator it does not.
func TestCheckinActionDisappearsWithoutAnAccount(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	if got := c.CheckinActions(context.Background()); len(got) != 0 {
		t.Fatalf("CheckinActions = %v, want none with no credential", got)
	}
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Checkin {
		t.Error("Capabilities.Checkin = false with no credential: CodeArts implements CheckinProvider")
	}
	if caps.CheckinReady {
		t.Error("Capabilities.CheckinReady = true with no credential, want no button on any row")
	}
}

func TestReviveAccountRejectsAnUnknownID(t *testing.T) {
	c := newTestClient(t, `{
		"base_url":"http://127.0.0.1:1",
		"access_key_id":"AK","secret_access_key":"SK","security_token":"TOK"}`)
	if err := c.ReviveAccount(context.Background(), "no-such-account"); err == nil {
		t.Fatal("reviving an unknown account reported success")
	}
	if err := c.ReviveAccount(context.Background(), c.pool.all()[0].id()); err != nil {
		t.Fatalf("reviving a real account: %v", err)
	}
}

func TestHintExplainsThisModuleOnly(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)

	auth := c.Hint(core.FailureAuth, "APIG.0602 invalid token", core.HintContext{})
	if !strings.Contains(auth, "security token") {
		t.Errorf("the auth hint = %q, want it to mention the temporary security token", auth)
	}
	quota := c.Hint(core.FailureQuota, "maas_type benefit refused", core.HintContext{})
	if !strings.Contains(quota, "free quota") {
		t.Errorf("the quota hint = %q, want it to explain the benefit header", quota)
	}
	// The reasoning_content 400 is a CodeArts-specific shape.
	reasoning := c.Hint(core.FailureOther, "Missing `reasoning_content` field", core.HintContext{})
	if !strings.Contains(reasoning, "reasoning_content") {
		t.Errorf("the reasoning hint = %q, want it to name the field", reasoning)
	}
	// A model the catalogue does not know is worth saying out loud.
	unknown := c.Hint(core.FailureOther, "something odd", core.HintContext{Model: "ghost-model"})
	if !strings.Contains(unknown, "ghost-model") {
		t.Errorf("the hint = %q, want it to name the unknown model", unknown)
	}
	// Anything else must defer to the shared table rather than invent advice.
	if got := c.Hint(core.FailureOther, "connection reset by peer", core.HintContext{}); got != "" {
		t.Errorf("Hint = %q, want an empty answer for a condition this module knows nothing about", got)
	}
}

// The catalogue cache is derived state and belongs in its own directory when
// one is configured, not beside the credentials.
func TestCatalogueCacheHonoursItsOwnDirectory(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	// The override is the reference implementation's own environment variable,
	// so an operator who already sets it gets the same behaviour.
	t.Setenv(envLegacyCacheDir, cache)
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	if c.modelsPath == "" || !strings.HasPrefix(c.modelsPath, cache) {
		t.Fatalf("modelsPath = %q, want it under %q", c.modelsPath, cache)
	}
	if c.accountsPath == "" || !strings.HasPrefix(c.accountsPath, c.deps.DataDir) {
		t.Fatalf("accountsPath = %q, want it under the data dir %q", c.accountsPath, c.deps.DataDir)
	}
	if strings.HasPrefix(c.accountsPath, cache) {
		t.Fatal("the credential store was placed in the cache directory")
	}
}

// A catalogue the vendor refuses must not cost the operator the list they
// already had.
func TestRefreshModelsKeepsTheLastGoodList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error_code":"APIG.0301"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, `{
		"base_url":`+jsonString(srv.URL)+`,
		"models_url":`+jsonString(srv.URL+"/models")+`,
		"gateway_url":`+jsonString(srv.URL+"/gw")+`,
		"access_key_id":"AK","secret_access_key":"SK","security_token":"TOK"}`)
	c.setModelsForTest([]core.Model{{ID: "kept-model"}}, time.Now().Add(-24*time.Hour))

	if _, err := c.RefreshModels(context.Background()); err == nil {
		t.Fatal("a refused catalogue reported success")
	}
	got := c.cachedModels()
	if len(got) != 1 || got[0].ID != "kept-model" {
		t.Fatalf("cachedModels = %v, want the last good list kept", got)
	}
}
