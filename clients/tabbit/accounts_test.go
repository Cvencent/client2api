package tabbit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// fakeSidecar is a stand-in for the real tabbit2api process: enough of /health
// and /v1/chat/completions that the module cannot tell the difference.  It runs
// on loopback, so the suite stays offline.
func fakeSidecar(t *testing.T, healthy bool) *httptest.Server {
	t.Helper()
	const stream = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"tabbit/priority\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if !healthy {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"version":"9.9.9","models":3,"web_host":"web.tabbit.ai"}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(stream))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func tabbitPanelClient(t *testing.T) *Client {
	t.Helper()
	clearTabbitEnv(t)
	withCandidates(t, closedPortURL(t))
	return newTestClient(t, "", nil)
}

func addEndpoint(t *testing.T, c *Client, fields map[string]string) core.AccountRecord {
	t.Helper()
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: fields})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	return rec
}

func TestTabbitAccountFieldsSchema(t *testing.T) {
	c := tabbitPanelClient(t)
	specs := c.AccountFields(context.Background())
	if len(specs) == 0 {
		t.Fatal("AccountFields returned nothing")
	}
	byKey := map[string]core.FieldSpec{}
	for _, s := range specs {
		if s.Key == "" || s.Label == "" {
			t.Errorf("incomplete field spec %#v", s)
		}
		if _, dup := byKey[s.Key]; dup {
			t.Errorf("duplicate field key %q", s.Key)
		}
		switch s.Type {
		case "text", "password", "textarea", "number", "bool", "select":
		default:
			t.Errorf("field %q has unsupported type %q", s.Key, s.Type)
		}
		byKey[s.Key] = s
	}
	bu, ok := byKey["base_url"]
	if !ok {
		t.Fatalf("no base_url field; got %v", byKey)
	}
	// base_url is no longer marked required: it belongs to the sidecar kind
	// only, and the panel would refuse a web-token account without one.  The
	// per-kind check lives in addSidecarAccount.
	if bu.Required {
		t.Error("base_url must not be required: a web-token account does not need one")
	}
	if key, ok := byKey["api_key"]; !ok {
		t.Error("no api_key field")
	} else if key.Type != "password" {
		t.Errorf("api_key type = %q, want password", key.Type)
	}

	// The kind selector is what makes the browser cookie addable by hand, so it
	// has to exist and to offer exactly the two transports this module speaks.
	kind, ok := byKey["kind"]
	if !ok {
		t.Fatalf("no kind field; got %v", byKey)
	}
	if kind.Type != "select" {
		t.Errorf("kind type = %q, want select", kind.Type)
	}
	if kind.Default != kindSidecar {
		t.Errorf("kind default = %q, want %q", kind.Default, kindSidecar)
	}
	want := map[string]bool{kindSidecar: true, kindWebToken: true}
	if len(kind.Options) != len(want) {
		t.Fatalf("kind options = %v, want %v", kind.Options, want)
	}
	for _, opt := range kind.Options {
		if !want[opt] {
			t.Errorf("unexpected kind option %q", opt)
		}
	}
	tok, ok := byKey["token"]
	if !ok {
		t.Fatal("no token field: a web-token account has to be addable by hand")
	}
	if tok.Type != "password" {
		t.Errorf("token type = %q, want password", tok.Type)
	}
}

func TestTabbitAddAccountRequiresBaseURL(t *testing.T) {
	c := tabbitPanelClient(t)
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{}); err == nil {
		t.Fatal("expected an error for an empty spec")
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"base_url": "   "}}); err == nil {
		t.Fatal("expected an error for a blank base_url")
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"base_url": "ftp://host:21"}}); err == nil {
		t.Fatal("expected an error for an unsupported scheme")
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"base_url": "http://"}}); err == nil {
		t.Fatal("expected an error for a URL with no host")
	}
}

// webJWTFor builds an unsigned JWT-shaped cookie.  Only the payload matters,
// because the module deliberately never verifies the signature.
func webJWTFor(t *testing.T, uid string, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims := map[string]any{"sub": uid, "iss": "https://web.tab-browser.com", "scope": "tab"}
	if !exp.IsZero() {
		claims["exp"] = exp.Unix()
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

// failingCLI is an executable that always exits non-zero, so the browser-cookie
// import can be made to fail without needing a Tabbit browser.
func failingCLI(t *testing.T) string {
	t.Helper()
	for _, p := range []string{`C:\Windows\System32\where.exe`, "/bin/false", "/usr/bin/false"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("no always-failing executable on this machine")
	return ""
}

func TestTabbitAddWebTokenAccount(t *testing.T) {
	ctx := context.Background()
	c := tabbitPanelClient(t)

	const uid = "041b492d-1d47-4ba0-9672-878a776bedde"
	token := webJWTFor(t, uid, time.Now().Add(24*time.Hour))
	rec, err := c.AddAccount(ctx, core.AccountSpec{Fields: map[string]string{
		"kind":  kindWebToken,
		"token": token,
	}})
	if err != nil {
		t.Fatalf("AddAccount(web-token): %v", err)
	}
	if rec.ID != webAccountIDTag+uid {
		t.Errorf("id = %q, want %q", rec.ID, webAccountIDTag+uid)
	}
	if rec.Fields["kind"] != kindWebToken {
		t.Errorf("kind = %v, want %s", rec.Fields["kind"], kindWebToken)
	}
	if rec.Fields["uid"] != uid {
		t.Errorf("uid = %v, want %s", rec.Fields["uid"], uid)
	}
	if rec.ExpiresAt == "" {
		t.Error("the expiry should have been reported")
	}
	// The credential itself must never travel back to the panel.
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if strings.Contains(string(blob), token) {
		t.Error("the record echoed the cookie")
	}

	list, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(list) != 1 || list[0].ID != rec.ID {
		t.Fatalf("Accounts = %+v, want the single web row", list)
	}

	// The row is what the web transport routes on.
	if !c.webRoute() {
		t.Error("a web-token account should make webRoute() true in auto mode")
	}
	ep, ok := c.firstEnabledWeb()
	if !ok || ep.Token != token {
		t.Errorf("firstEnabledWeb = %+v, %v; want the imported cookie", ep, ok)
	}
}

func TestTabbitAddWebTokenRejectsBadCookies(t *testing.T) {
	ctx := context.Background()
	c := tabbitPanelClient(t)

	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"blank", "   ", "token is required"},
		{"not a jwt", "hello", "cannot be used"},
		{"expired", webJWTFor(t, "u1", time.Now().Add(-time.Hour)), "expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.AddAccount(ctx, core.AccountSpec{Fields: map[string]string{
				"kind":  kindWebToken,
				"token": tc.token,
			}})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestTabbitAddAccountRejectsUnknownKind(t *testing.T) {
	c := tabbitPanelClient(t)
	_, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"kind": "telepathy", "base_url": "127.0.0.1:50124"},
	})
	if err == nil {
		t.Fatal("expected an error for an unknown kind")
	}
	if !strings.Contains(err.Error(), "unknown account kind") {
		t.Errorf("error = %q, want it to name the unknown kind", err)
	}
}

func TestTabbitImportAllSurvivesABrokenCookieSource(t *testing.T) {
	ctx := context.Background()
	srv := fakeSidecar(t, true)
	clearTabbitEnv(t)
	withCandidates(t, srv.URL)
	c := newTestClient(t, `{"tabbit_cli":"`+filepath.ToSlash(failingCLI(t))+`"}`, nil)

	// The launcher exists, so Discover offers the cookie as importable...
	found, err := c.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var cookie *core.DiscoveredCredential
	for i := range found {
		if found[i].Path == browserCookiePath {
			cookie = &found[i]
		}
	}
	if cookie == nil {
		t.Fatalf("the browser cookie is not offered at all: %+v", found)
	}
	if !cookie.Importable {
		t.Fatalf("the browser cookie should be offered as importable: %+v", cookie)
	}

	// ...but a sweep still imports the sidecar instead of failing outright,
	// because one unreachable source must not discard the others.
	got, err := c.Import(ctx, nil, true)
	if err != nil {
		t.Fatalf("Import all: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("imported %d accounts, want the one sidecar", len(got))
	}

	// Naming the cookie explicitly is a request, so its failure is reported.
	if _, err := c.Import(ctx, []string{browserCookiePath}, false); err == nil {
		t.Error("an explicit browser-cookie import should surface the launcher error")
	}
}

func TestTabbitAddAccountNormalizesAndPersists(t *testing.T) {
	ctx := context.Background()
	c := tabbitPanelClient(t)

	// A bare host:port is what a human actually types.
	rec := addEndpoint(t, c, map[string]string{"base_url": "127.0.0.1:50124/", "label": "local"})
	if rec.ID != "http://127.0.0.1:50124" {
		t.Fatalf("id = %q, want the canonical URL", rec.ID)
	}
	if rec.Label != "local" {
		t.Errorf("label = %q, want local", rec.Label)
	}
	if !rec.Enabled {
		t.Error("a freshly added endpoint should be enabled")
	}

	raw, err := os.ReadFile(filepath.Join(c.deps.DataDir, accountsFileName))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if !strings.Contains(string(raw), "http://127.0.0.1:50124") {
		t.Error("endpoint was not persisted")
	}

	list, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	var found *core.AccountRecord
	for i := range list {
		if list[i].ID == rec.ID {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatalf("added endpoint %q missing from Accounts", rec.ID)
	}
	if found.Fields["managed"] != true {
		t.Errorf("managed = %#v, want true", found.Fields["managed"])
	}
}

func TestTabbitAddAccountIsIdempotent(t *testing.T) {
	c := tabbitPanelClient(t)
	first := addEndpoint(t, c, map[string]string{"base_url": "127.0.0.1:50124", "label": "one"})
	second := addEndpoint(t, c, map[string]string{"base_url": "http://127.0.0.1:50124", "label": "two"})

	if first.ID != second.ID {
		t.Fatalf("ids differ: %q vs %q", first.ID, second.ID)
	}
	list, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	n := 0
	for _, r := range list {
		if r.ID == first.ID {
			n++
			if r.Label != "two" {
				t.Errorf("label = %q, want the updated label", r.Label)
			}
		}
	}
	if n != 1 {
		t.Fatalf("the same endpoint appears %d times, want 1", n)
	}
}

// The account table is the panel's editing surface: every row it shows gets
// 删除 / 停用 buttons, and both of those only ever work on an endpoint the panel
// added.  So an endpoint this module merely RESOLVES (config, environment, the
// hand-off file, or the built-in default) must not be listed — otherwise the
// pool carries a row whose only two buttons can do nothing but fail.
func TestTabbitAccountsListsOnlyEndpointsThePanelAdded(t *testing.T) {
	ctx := context.Background()
	c := tabbitPanelClient(t)

	list, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("a client with no stored endpoint listed %d account(s): %+v", len(list), list)
	}

	// Nothing is hidden: which endpoint the module would use, and why, is still
	// reported — on the status surface rather than as a fake account.
	loc := c.locate()
	if loc.baseURL == "" {
		t.Fatal("locate() resolved nothing, so the status check below would prove nothing")
	}
	st := c.Status(ctx)
	if len(st.Accounts) == 0 || st.Accounts[0].ID != loc.baseURL {
		t.Fatalf("Status() no longer reports the resolved endpoint %q: %+v", loc.baseURL, st.Accounts)
	}
	if !strings.Contains(st.Detail, loc.baseURL) {
		t.Errorf("Status().Detail does not mention %q: %q", loc.baseURL, st.Detail)
	}

	rec := addEndpoint(t, c, map[string]string{"base_url": "127.0.0.1:50124"})
	list, err = c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(list) != 1 || list[0].ID != rec.ID {
		t.Fatalf("after adding one endpoint the pool listed %d row(s): %+v", len(list), list)
	}
	if list[0].Fields["managed"] != true {
		t.Errorf("managed = %#v, want true: the panel offers 删除 for this row", list[0].Fields["managed"])
	}

	// Removing it must leave the pool empty: nothing may reappear from the
	// resolver as an undeletable row.
	if err := c.RemoveAccount(ctx, rec.ID); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	list, err = c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("after removing the endpoint the pool listed %d row(s): %+v", len(list), list)
	}
}

func TestTabbitSetAccountEnabledAndRemove(t *testing.T) {
	ctx := context.Background()
	c := tabbitPanelClient(t)
	rec := addEndpoint(t, c, map[string]string{"base_url": "127.0.0.1:50124"})

	if err := c.SetAccountEnabled(ctx, rec.ID, false); err != nil {
		t.Fatalf("SetAccountEnabled(false): %v", err)
	}
	list, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	var found *core.AccountRecord
	for i := range list {
		if list[i].ID == rec.ID {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatal("a disabled endpoint must stay listed so it can be switched back on")
	}
	if found.Enabled {
		t.Error("disabled endpoint still reported as enabled")
	}
	if err := c.SetAccountEnabled(ctx, "ghost", false); err == nil {
		t.Error("expected an error for an unknown id")
	}
	if err := c.SetAccountEnabled(ctx, rec.ID, true); err != nil {
		t.Fatalf("SetAccountEnabled(true): %v", err)
	}

	if err := c.RemoveAccount(ctx, rec.ID); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if err := c.RemoveAccount(ctx, rec.ID); err == nil {
		t.Error("expected an error removing an unknown id")
	}
	list, err = c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	for _, r := range list {
		if r.ID == rec.ID {
			t.Fatalf("removed endpoint %q still listed", rec.ID)
		}
	}
}

func TestTabbitStoredEndpointWinsInLocate(t *testing.T) {
	c := tabbitPanelClient(t)
	rec := addEndpoint(t, c, map[string]string{"base_url": "127.0.0.1:50124"})

	loc := c.locate()
	if loc.baseURL != rec.ID {
		t.Errorf("locate().baseURL = %q, want %q", loc.baseURL, rec.ID)
	}
	if loc.source != epOriginPanel {
		t.Errorf("locate().source = %q, want %q", loc.source, epOriginPanel)
	}
	if !loc.explicit() {
		t.Error("a panel endpoint must count as explicit so resolve() uses it as-is")
	}

	// Disabling it hands resolution back to the default probe.
	if err := c.SetAccountEnabled(context.Background(), rec.ID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	if got := c.locate(); got.source == epOriginPanel {
		t.Errorf("a disabled endpoint is still winning: %#v", got)
	}
}

func TestTabbitTestAccountLive(t *testing.T) {
	srv := fakeSidecar(t, true)
	c := tabbitPanelClient(t)
	rec := addEndpoint(t, c, map[string]string{"base_url": srv.URL})

	res, err := c.TestAccount(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false, error = %q", res.Error)
	}
	if res.Reply != "pong" {
		t.Errorf("Reply = %q, want pong", res.Reply)
	}
	if res.Model == "" {
		t.Error("Model is empty")
	}
	if res.AccountID != rec.ID {
		t.Errorf("AccountID = %q, want %q", res.AccountID, rec.ID)
	}
}

func TestTabbitTestAccountDeadEndpoint(t *testing.T) {
	c := tabbitPanelClient(t)
	rec := addEndpoint(t, c, map[string]string{"base_url": closedPortURL(t)})

	res, err := c.TestAccount(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("a dead endpoint must be a result, not a Go error: %v", err)
	}
	if res.OK {
		t.Fatal("OK = true for a closed port")
	}
	if strings.TrimSpace(res.Error) == "" {
		t.Error("Error is empty")
	}
}

func TestTabbitTestAccountUnhealthySidecar(t *testing.T) {
	srv := fakeSidecar(t, false)
	c := tabbitPanelClient(t)
	rec := addEndpoint(t, c, map[string]string{"base_url": srv.URL})

	res, err := c.TestAccount(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if res.OK {
		t.Fatal("OK = true although /health returned 500")
	}
	if !strings.Contains(res.Error, "did not answer") {
		t.Errorf("Error = %q, want it to mention the health probe", res.Error)
	}
}

func TestTabbitTestAccountUnknownID(t *testing.T) {
	c := tabbitPanelClient(t)
	if _, err := c.TestAccount(context.Background(), "ghost"); err == nil {
		t.Error("expected an error for an unknown id")
	}
}

func TestTabbitRefreshAccountReportsReachability(t *testing.T) {
	ctx := context.Background()
	c := tabbitPanelClient(t)

	live := addEndpoint(t, c, map[string]string{"base_url": fakeSidecar(t, true).URL})
	dead := addEndpoint(t, c, map[string]string{"base_url": closedPortURL(t)})

	results, err := c.RefreshAccount(ctx, "")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	byID := map[string]core.RefreshResult{}
	for _, r := range results {
		byID[r.AccountID] = r
	}
	if !byID[live.ID].OK {
		t.Errorf("live endpoint reported not OK: %+v", byID[live.ID])
	}
	if byID[dead.ID].OK {
		t.Errorf("dead endpoint reported OK: %+v", byID[dead.ID])
	}
	if _, err := c.RefreshAccount(ctx, "ghost"); err == nil {
		t.Error("expected an error for an unknown id")
	}
}

func TestTabbitDiscoverAndImport(t *testing.T) {
	ctx := context.Background()
	srv := fakeSidecar(t, true)
	clearTabbitEnv(t)
	withCandidates(t, srv.URL, closedPortURL(t))
	c := newTestClient(t, "", nil)

	found, err := c.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var live, dead *core.DiscoveredCredential
	for i := range found {
		switch found[i].Path {
		case srv.URL:
			live = &found[i]
		default:
			dead = &found[i]
		}
	}
	if live == nil {
		t.Fatalf("the live sidecar was not discovered: %#v", found)
	}
	if !live.Importable {
		t.Errorf("live sidecar is not importable: %s", live.Note)
	}
	if live.Imported {
		t.Error("Imported should be false before Import runs")
	}
	if dead != nil && dead.Importable {
		t.Errorf("a closed port was reported as importable: %s", dead.Note)
	}

	imported, err := c.Import(ctx, []string{srv.URL}, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(imported) != 1 {
		t.Fatalf("imported %d endpoints, want 1", len(imported))
	}

	again, err := c.Discover(ctx)
	if err != nil {
		t.Fatalf("second Discover: %v", err)
	}
	for _, d := range again {
		if d.Path == srv.URL && !d.Imported {
			t.Error("Imported is still false after a successful import")
		}
	}
}

func TestTabbitImportAllSkipsAlreadyImported(t *testing.T) {
	ctx := context.Background()
	srv := fakeSidecar(t, true)
	clearTabbitEnv(t)
	withCandidates(t, srv.URL)
	// Point the launcher at a path that does not exist so the browser-cookie
	// source is not importable: this test is about the sidecar sweep and must
	// not depend on whether a Tabbit browser is installed on the machine.
	missing := filepath.ToSlash(filepath.Join(t.TempDir(), "no-such-tabbit-cli.exe"))
	c := newTestClient(t, `{"tabbit_cli":"`+missing+`"}`, nil)

	first, err := c.Import(ctx, nil, true)
	if err != nil {
		t.Fatalf("Import all: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("imported %d endpoints, want 1", len(first))
	}
	second, err := c.Import(ctx, nil, true)
	if err == nil && len(second) != 0 {
		t.Fatalf("second Import all added %d endpoints, want 0", len(second))
	}
}

func TestTabbitImportRejectsEmpty(t *testing.T) {
	c := tabbitPanelClient(t)
	if _, err := c.Import(context.Background(), nil, false); err == nil {
		t.Error("expected an error when nothing was named and all is false")
	}
}

func TestTabbitAccountsNeverLeakKey(t *testing.T) {
	c := tabbitPanelClient(t)
	addEndpoint(t, c, map[string]string{
		"base_url": "127.0.0.1:50124",
		"api_key":  "SUPER_SECRET_BEARER_XYZ",
	})

	list, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	blob, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "SUPER_SECRET_BEARER_XYZ") {
		t.Fatalf("Accounts leaked the bearer: %s", blob)
	}
}

func TestTabbitStoreSurvivesReload(t *testing.T) {
	ctx := context.Background()
	c := tabbitPanelClient(t)
	rec := addEndpoint(t, c, map[string]string{"base_url": "127.0.0.1:50124", "api_key": "k"})

	// A second client over the same DataDir must see the same store, which is
	// what makes the panel's changes survive a restart.
	reopened, err := New(core.Deps{
		DataDir: c.deps.DataDir,
		Logf:    func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	rc, ok := reopened.(*Client)
	if !ok {
		t.Fatalf("New returned %T", reopened)
	}
	list, err := rc.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	for _, r := range list {
		if r.ID == rec.ID {
			return
		}
	}
	t.Fatalf("reopened client lost endpoint %q", rec.ID)
}
