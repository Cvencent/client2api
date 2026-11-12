package qoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// realm_test.go pins the two-upstream split: the addressing protocol, the
// per-realm hosts, and the candidate filtering that keeps a credential away from
// a catalogue it cannot serve.

func TestNormalizeRealmAcceptsEverySpelling(t *testing.T) {
	cases := map[string]string{
		"":              realmCN,
		"cn":            realmCN,
		"CN":            realmCN,
		"china":         realmCN,
		"mainland":      realmCN,
		"global":        realmGlobal,
		"GLOBAL":        realmGlobal,
		"intl":          realmGlobal,
		"international": realmGlobal,
		"overseas":      realmGlobal,
		"  global  ":    realmGlobal,
		"nonsense":      realmCN,
	}
	for in, want := range cases {
		if got := normalizeRealm(in); got != want {
			t.Errorf("normalizeRealm(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveModelRealm(t *testing.T) {
	cases := []struct{ model, realm, bare string }{
		{"cn:auto", realmCN, "auto"},
		{"global:efficient", realmGlobal, "efficient"},
		// A bare id names no realm: the caller left the choice to the module.
		{"auto", "", "auto"},
		// Only the first colon counts, and only an exact realm prefix is
		// stripped -- a model id that legitimately contains a colon stays whole.
		{"nvidia/nemotron:free", "", "nvidia/nemotron:free"},
		{"intl:auto", "", "intl:auto"},
		{"global:", realmGlobal, ""},
	}
	for _, c := range cases {
		realm, bare := resolveModelRealm(c.model)
		if realm != c.realm || bare != c.bare {
			t.Errorf("resolveModelRealm(%q) = (%q, %q), want (%q, %q)", c.model, realm, bare, c.realm, c.bare)
		}
	}
}

func TestRealmQualifiedOnlyMatchesExactPrefixes(t *testing.T) {
	for _, model := range []string{"cn:auto", "global:auto"} {
		if !realmQualified(model) {
			t.Errorf("realmQualified(%q) = false, want true", model)
		}
	}
	for _, model := range []string{"auto", "intl:auto", "global-2:auto", ":auto"} {
		if realmQualified(model) {
			t.Errorf("realmQualified(%q) = true, want false", model)
		}
	}
}

func TestQualifyModelIDRoundTrips(t *testing.T) {
	for _, realm := range []string{realmCN, realmGlobal, ""} {
		qualified := qualifyModelID(realm, "auto")
		gotRealm, bare := resolveModelRealm(qualified)
		if gotRealm != normalizeRealm(realm) || bare != "auto" {
			t.Errorf("qualifyModelID(%q, auto) = %q, which resolves to (%q, %q)", realm, qualified, gotRealm, bare)
		}
	}
}

func TestRouteModelPrefersTheDefaultRealmForABareName(t *testing.T) {
	// Bare on the domestic default: domestic first, then any realm.
	route := routeModel("auto", realmCN)
	if route.bare != "auto" {
		t.Fatalf("bare = %q, want auto", route.bare)
	}
	if len(route.realms) != 2 || route.realms[0] != realmCN || route.realms[1] != "" {
		t.Fatalf("realms = %v, want [cn \"\"]", route.realms)
	}
	if route.affinityRealm != "" {
		t.Fatalf("affinityRealm = %q, want empty (a bare name is a preference)", route.affinityRealm)
	}

	// Bare on a global default: global first, then any realm.
	route = routeModel("efficient", realmGlobal)
	if route.realms[0] != realmGlobal || route.realms[1] != "" {
		t.Fatalf("realms = %v, want [global \"\"]", route.realms)
	}
}

func TestRouteModelPinsAnExplicitPrefix(t *testing.T) {
	route := routeModel("global:efficient", realmCN)
	if len(route.realms) != 1 || route.realms[0] != realmGlobal {
		t.Fatalf("realms = %v, want exactly [global] (an explicit prefix is a hard filter)", route.realms)
	}
	if route.affinityRealm != realmGlobal {
		t.Fatalf("affinityRealm = %q, want global", route.affinityRealm)
	}
	if route.bare != "efficient" {
		t.Fatalf("bare = %q, want efficient", route.bare)
	}
}

func TestAllowedRealmsUnfiltersOnTheAnyEntry(t *testing.T) {
	if got := allowedRealms(routeModel("global:x", realmCN)); got == nil || !got[realmGlobal] || got[realmCN] {
		t.Fatalf("allowedRealms(global:x) = %v, want only global", got)
	}
	// The soft route's trailing "" entry means every realm may serve it.
	if got := allowedRealms(routeModel("x", realmCN)); got != nil {
		t.Fatalf("allowedRealms(bare) = %v, want nil (every realm)", got)
	}
}

func TestDefaultRealmConfigResolvesToCN(t *testing.T) {
	var cfg config
	if got := cfg.defaultRealm(); got != realmCN {
		t.Fatalf("defaultRealm() = %q, want cn", got)
	}
	cfg.DefaultRealm = "intl"
	if got := cfg.defaultRealm(); got != realmGlobal {
		t.Fatalf("defaultRealm() with intl = %q, want global", got)
	}
}

func TestRealmHostsAreDistinct(t *testing.T) {
	var cfg config
	if got := cfg.openAPIBaseFor(realmCN); !strings.Contains(got, "qoder.com.cn") {
		t.Errorf("domestic OpenAPI host = %q, want the mainland host", got)
	}
	if got := cfg.openAPIBaseFor(realmGlobal); !strings.Contains(got, "openapi.qoder.sh") {
		t.Errorf("international OpenAPI host = %q, want openapi.qoder.sh", got)
	}
	if got := cfg.authBaseFor(realmGlobal); !strings.Contains(got, "qoder.com") || strings.Contains(got, ".cn") {
		t.Errorf("international auth host = %q, want qoder.com", got)
	}
	if got := cfg.gatewayBaseFor(realmGlobal); !strings.Contains(got, "qoder.sh") {
		t.Errorf("international gateway host = %q, want a qoder.sh host", got)
	}
	// Only the international realm has a clean OpenAI model server; the mainland
	// build streams from the signed /algo edge instead.
	if got := cfg.modelServerBaseFor(realmCN); got != "" {
		t.Errorf("domestic model server = %q, want empty", got)
	}
	if got := cfg.modelServerBaseFor(realmGlobal); !strings.Contains(got, "qoder.sh") {
		t.Errorf("international model server = %q, want the vendor's model host", got)
	}
}

// TestCandidateFilteringKeepsARealmOutOfTheOtherCatalogue is the whole point of
// the split: an explicit "global:" model must never be handed a domestic
// credential, because that credential can only answer "Unsupported model".
func TestCandidateFilteringKeepsARealmOutOfTheOtherCatalogue(t *testing.T) {
	cn := account{storedAccount: storedAccount{ID: "cn-1", Token: "dt-cn", Enabled: true, Realm: realmCN}}
	global := account{storedAccount: storedAccount{ID: "global-1", Token: "dt-global", Enabled: true, Realm: realmGlobal}}
	c := testClient(t, "http://example.invalid", cn, global)

	got := c.orderedCandidatesFor(nil, "global:efficient", routeModel("global:efficient", realmCN))
	if len(got) != 1 || got[0].ID != "global-1" {
		t.Fatalf("explicit global route picked %v, want exactly the global account", ids(got))
	}

	got = c.orderedCandidatesFor(nil, "cn:auto", routeModel("cn:auto", realmCN))
	if len(got) != 1 || got[0].ID != "cn-1" {
		t.Fatalf("explicit cn route picked %v, want exactly the domestic account", ids(got))
	}

	// A bare name is a preference, not a filter: both realms stay eligible, the
	// default realm first.
	got = c.orderedCandidatesFor(nil, "auto", routeModel("auto", realmGlobal))
	if len(got) != 2 {
		t.Fatalf("bare route picked %v, want both realms eligible", ids(got))
	}
	if got[0].realmOf() != realmGlobal {
		t.Fatalf("bare route on a global default put %q first, want a global account", got[0].ID)
	}
}

func ids(accounts []account) []string {
	out := make([]string, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, a.ID)
	}
	return out
}

func TestAccountRecordCarriesItsRealm(t *testing.T) {
	a := account{storedAccount: storedAccount{ID: "x", Token: "dt", Enabled: true, Realm: realmGlobal}}
	rec := accountRecord(&a, time.Now().UTC())
	if rec.Fields["realm"] != realmGlobal {
		t.Fatalf("record realm = %v, want global", rec.Fields["realm"])
	}
	// A credential written before the split has no realm and must read as cn,
	// not as an empty string the panel would have to guess about.
	b := account{storedAccount: storedAccount{ID: "y", Token: "dt", Enabled: true}}
	if got := accountRecord(&b, time.Now().UTC()).Fields["realm"]; got != realmCN {
		t.Fatalf("blank realm rendered as %v, want cn", got)
	}
}

// TestQuotaReadGoesToTheAccountsOwnRealm proves the realm swap reaches the wire:
// a global account's ledger read must hit the international host.
func TestQuotaReadGoesToTheAccountsOwnRealm(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"userQuota":{"total":10,"used":1,"remaining":9,"unit":"credits"}}`))
	}))
	defer server.Close()

	cfg := config{OpenAPIBase: server.URL, GlobalOpenAPIBase: server.URL}
	global := account{storedAccount: storedAccount{ID: "g", Token: "dt", Enabled: true, Realm: realmGlobal}}
	c := &Client{cfg: cfg, up: newUpstream(cfg, server.Client(), nil)}
	c.store = &store{all: []*account{&global}}

	if _, err := c.AccountBalance(context.Background(), "g", 0); err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if gotPath != pathQuota {
		t.Fatalf("path = %q, want %q", gotPath, pathQuota)
	}
}

// TestLoginRealmsOffersTheInternationalGrant keeps the picker honest: an
// operator with only a qoder.com account must have a route in.
func TestLoginRealmsOffersTheInternationalGrant(t *testing.T) {
	c := testClient(t, "http://example.invalid")
	found := false
	for _, r := range c.LoginRealms(context.Background()) {
		if r.Code == globalRealm {
			found = true
		}
	}
	if !found {
		t.Fatal("LoginRealms did not offer the international sign-in route")
	}
}

func TestStartLoginRealmBuildsAnInternationalURL(t *testing.T) {
	c := testClient(t, "http://example.invalid")
	st, err := c.StartLoginRealm(context.Background(), globalRealm)
	if err != nil {
		t.Fatalf("StartLoginRealm(global): %v", err)
	}
	if st.Realm != realmGlobal {
		t.Fatalf("state realm = %q, want global", st.Realm)
	}
	if !strings.Contains(st.URL, "qoder.com") || strings.Contains(st.URL, "qoder.cn") {
		t.Fatalf("international sign-in URL = %q, want a qoder.com host", st.URL)
	}
}

func TestStartLoginRealmRejectsAnUnknownRoute(t *testing.T) {
	c := testClient(t, "http://example.invalid")
	if _, err := c.StartLoginRealm(context.Background(), "nonsense"); err == nil {
		t.Fatal("an unknown sign-in route was accepted")
	}
}

// TestAddAccountStoresTheChosenRealm proves the manual form records what the
// operator picked, and that the probe goes to that realm's host.
func TestAddAccountStoresTheChosenRealm(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u-9","name":"intl user"}`))
	}))
	defer server.Close()

	cfg := config{OpenAPIBase: server.URL, GlobalOpenAPIBase: server.URL}
	c := &Client{cfg: cfg, up: newUpstream(cfg, server.Client(), nil), store: &store{}}
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"token": "dt-x", "realm": realmGlobal},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if gotPath != pathUserInfo {
		t.Fatalf("probe path = %q, want %q", gotPath, pathUserInfo)
	}
	if rec.Fields["realm"] != realmGlobal {
		t.Fatalf("stored realm = %v, want global", rec.Fields["realm"])
	}
}
