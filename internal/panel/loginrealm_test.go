package panel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// fakeRealmLoginClient is a module whose "add an account" is a choice between
// two vendor services -- the shape workbuddy has, where the two realms keep
// separate credential stores and serve separate model catalogues.
type fakeRealmLoginClient struct {
	*fakeAccountClient

	realms []core.LoginRealm

	mu       sync.Mutex
	started  []string
	sessions int
}

func realmLoginClient(name string, realms ...core.LoginRealm) *fakeRealmLoginClient {
	return &fakeRealmLoginClient{
		fakeAccountClient: &fakeAccountClient{fakeClient: &fakeClient{name: name}},
		realms:            realms,
	}
}

func (f *fakeRealmLoginClient) LoginRealms(context.Context) []core.LoginRealm { return f.realms }

func (f *fakeRealmLoginClient) StartLogin(ctx context.Context) (core.LoginState, error) {
	return f.begin("")
}

func (f *fakeRealmLoginClient) StartLoginRealm(_ context.Context, realm string) (core.LoginState, error) {
	return f.begin(realm)
}

func (f *fakeRealmLoginClient) begin(realm string) (core.LoginState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, realm)
	f.sessions++
	id := fmt.Sprintf("sess-%d", f.sessions)
	return core.LoginState{
		SessionID: id,
		State:     core.LoginPending,
		URL:       "https://example.test/auth?state=" + id,
		Realm:     realm,
	}, nil
}

func (f *fakeRealmLoginClient) PollLogin(context.Context, string) (core.LoginState, error) {
	return core.LoginState{State: core.LoginPending}, nil
}

func (f *fakeRealmLoginClient) CancelLogin(context.Context, string) error { return nil }

// began is the realms the module was actually asked for, in order.  The point
// of every test below is what arrives here, not what the route echoed back.
func (f *fakeRealmLoginClient) began() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

// fakePlainLoginClient is a module with one login flow and no realm choice --
// the shape every module had before the picker existed.
type fakePlainLoginClient struct {
	*fakeAccountClient

	mu     sync.Mutex
	starts int
}

func plainLoginClient(name string) *fakePlainLoginClient {
	return &fakePlainLoginClient{
		fakeAccountClient: &fakeAccountClient{fakeClient: &fakeClient{name: name}},
	}
}

func (f *fakePlainLoginClient) StartLogin(context.Context) (core.LoginState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	return core.LoginState{
		SessionID: "sess-1",
		State:     core.LoginPending,
		URL:       "https://example.test/auth",
	}, nil
}

func (f *fakePlainLoginClient) PollLogin(context.Context, string) (core.LoginState, error) {
	return core.LoginState{State: core.LoginPending}, nil
}

func (f *fakePlainLoginClient) CancelLogin(context.Context, string) error { return nil }

func (f *fakePlainLoginClient) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

func loginPanel(t *testing.T, clients ...core.Client) http.Handler {
	t.Helper()
	return New(Options{
		Registry: registryOf(clients...),
		Version:  "test",
		Listen:   "127.0.0.1:0",
		Started:  time.Now(),
	})
}

// TestLoginRegionsReportsWhatTheModuleOffers: the picker is rendered from the
// capability report, so the dedicated route and /capabilities have to agree.  A
// disagreement would show the operator a choice the module cannot honour.
func TestLoginRegionsReportsWhatTheModuleOffers(t *testing.T) {
	c := realmLoginClient("verbs",
		core.LoginRealm{Code: "cn", Name: "国内版", Help: "国内"},
		core.LoginRealm{Code: "global", Name: "国际版", Help: "国际"},
	)
	h := loginPanel(t, c)

	rec := hitRoute(t, h, http.MethodGet, "/panel/api/clients/verbs/login/regions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)
	if body["client"] != "verbs" {
		t.Errorf("client = %v, want verbs", body["client"])
	}
	realms, ok := body["realms"].([]any)
	if !ok || len(realms) != 2 {
		t.Fatalf("realms = %v, want exactly two", body["realms"])
	}
	first, _ := realms[0].(map[string]any)
	if first["code"] != "cn" || first["name"] != "国内版" {
		t.Errorf("first realm = %v, want cn/国内版 in the module's own order", realms[0])
	}

	caps := decodeMap(t, hitRoute(t, h, http.MethodGet, "/panel/api/clients/verbs/capabilities", ""))
	if login, _ := caps["login"].(bool); !login {
		t.Errorf("capabilities did not report a login at all: %v", caps)
	}
	if got, _ := caps["realms"].([]any); len(got) != 2 {
		t.Errorf("capabilities carried %v, want the same two realms", caps["realms"])
	}
}

// TestLoginRegionsIsEmptyForASingleRealmModule: the honest answer for a module
// with one upstream is an empty list, which is what tells the panel to render no
// picker rather than a picker with nothing in it.  It must be [] and not null,
// or the frontend has to special-case two kinds of "nothing".
func TestLoginRegionsIsEmptyForASingleRealmModule(t *testing.T) {
	h := loginPanel(t, plainLoginClient("verbs"))

	rec := hitRoute(t, h, http.MethodGet, "/panel/api/clients/verbs/login/regions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)
	realms, ok := body["realms"].([]any)
	if !ok {
		t.Fatalf("realms = %v (%T), want an empty array rather than null", body["realms"], body["realms"])
	}
	if len(realms) != 0 {
		t.Errorf("realms = %v, want none", realms)
	}
}

// TestLoginRegionsIsNotMistakenForASession pins the route order.  "regions" sits
// exactly where a session id sits, and the session handler would happily answer
// for it -- returning a poll state instead of the realm list, with no error
// anywhere to notice.
func TestLoginRegionsIsNotMistakenForASession(t *testing.T) {
	h := loginPanel(t, realmLoginClient("verbs", core.LoginRealm{Code: "cn", Name: "国内版"}))

	rec := hitRoute(t, h, http.MethodGet, "/panel/api/clients/verbs/login/regions", "")
	body := decodeMap(t, rec)
	if _, isPoll := body["state"]; isPoll {
		t.Fatalf("login/regions was served by the session handler: %s", rec.Body.String())
	}
	if _, ok := body["realms"]; !ok {
		t.Fatalf("login/regions did not answer with realms: %v", body)
	}

	// A real session id must still reach the poll handler.
	rec = hitRoute(t, h, http.MethodGet, "/panel/api/clients/verbs/login/sess-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("session poll status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := decodeMap(t, rec)["state"]; !ok {
		t.Errorf("a real session id did not reach the poll handler: %s", rec.Body.String())
	}
}

// TestStartLoginCarriesThePickedRealm is the whole point of the route: the
// operator's choice has to reach the module.  Without this the picker renders,
// accepts the choice and silently ignores it, landing the credential on the
// configured default realm -- the one outcome worse than not offering a choice.
func TestStartLoginCarriesThePickedRealm(t *testing.T) {
	c := realmLoginClient("verbs",
		core.LoginRealm{Code: "cn", Name: "国内版"},
		core.LoginRealm{Code: "global", Name: "国际版"},
	)
	h := loginPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/verbs/login", `{"realm":"global"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := c.began(); len(got) != 1 || got[0] != "global" {
		t.Fatalf("module saw realms %v, want exactly [global]", got)
	}
	if got := decodeMap(t, rec)["realm"]; got != "global" {
		t.Errorf("response realm = %v, want global so the panel can label the wait", got)
	}
}

// TestStartLoginWithoutABodyKeepsTheOldBehaviour: every caller that predates the
// picker posts an empty body.  Adding a picker must not make those callers start
// failing, and it must not turn "no choice made" into a choice.
func TestStartLoginWithoutABodyKeepsTheOldBehaviour(t *testing.T) {
	c := realmLoginClient("verbs", core.LoginRealm{Code: "cn", Name: "国内版"})
	h := loginPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/verbs/login", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := c.began(); len(got) != 1 || got[0] != "" {
		t.Fatalf("module saw realms %v, want one empty realm (the configured default)", got)
	}
}

// TestStartLoginRefusesAnUnreadableBody: silently defaulting would add the
// account to whichever realm the config names, which is the exact mistake the
// picker exists to prevent -- so a body we cannot parse is a refusal, not a
// fallback, and no login may start.
func TestStartLoginRefusesAnUnreadableBody(t *testing.T) {
	c := realmLoginClient("verbs", core.LoginRealm{Code: "cn", Name: "国内版"})
	h := loginPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/verbs/login", `realm=global`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if got := c.began(); len(got) != 0 {
		t.Fatalf("a login started anyway (%v) after an unreadable body", got)
	}
	if !strings.Contains(rec.Body.String(), "realm") {
		t.Errorf("the refusal does not say what a valid body looks like: %s", rec.Body.String())
	}
}

// TestStartLoginRealmIsIgnoredByAModuleWithNoRealmSupport: a module that never
// opted in must behave exactly as before even when a caller sends a realm, and
// it must not echo a realm back -- there is nothing it could have honoured.
func TestStartLoginRealmIsIgnoredByAModuleWithNoRealmSupport(t *testing.T) {
	c := plainLoginClient("verbs")
	h := loginPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/verbs/login", `{"realm":"global"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := c.startCount(); got != 1 {
		t.Fatalf("the plain StartLogin ran %d times, want 1", got)
	}
	if got := decodeMap(t, rec)["realm"]; got != nil {
		t.Errorf("a single-realm module reported realm %v, want none", got)
	}
}

// TestLoginRegionsOnlyAnswersGET keeps the read-only route from doubling as a
// login trigger: starting a flow is what POST /login is for.
func TestLoginRegionsOnlyAnswersGET(t *testing.T) {
	h := loginPanel(t, realmLoginClient("verbs", core.LoginRealm{Code: "cn", Name: "国内版"}))

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/verbs/login/regions", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST login/regions = %d, want 405", rec.Code)
	}
}
