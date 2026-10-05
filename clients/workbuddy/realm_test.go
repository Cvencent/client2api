package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// --- realm parsing ----------------------------------------------------------
//
// The realm qualifier is an addressing protocol, so its parser is the one place
// a wrong guess turns into a 11102 "model not found" for a model that plainly
// exists in the other catalogue.  These cases pin the reference's rules: only
// the first colon counts, the prefix is compared case-sensitively against
// exactly "cn" and "global", and anything else is left whole.

func TestWorkbuddyResolveModelRealm(t *testing.T) {
	tests := []struct {
		in    string
		realm string
		bare  string
	}{
		{"glm-5.2", realmCN, "glm-5.2"},
		{"cn:glm-5.2", realmCN, "glm-5.2"},
		{"global:glm-5.2", realmGlobal, "glm-5.2"},
		{"  global:glm-5.2  ", realmGlobal, "glm-5.2"},
		// A model id may legitimately contain a colon; only the first one is a
		// realm separator and only the exact prefixes are protocol.
		{"cn:global:glm-5.2", realmCN, "global:glm-5.2"},
		{"intl:glm-5.2", realmCN, "intl:glm-5.2"},
		{"CN:glm-5.2", realmCN, "CN:glm-5.2"},
		{"Global:glm-5.2", realmCN, "Global:glm-5.2"},
		{":glm-5.2", realmCN, ":glm-5.2"},
		{"cn:", realmCN, ""},
		{"", realmCN, ""},
		{"   ", realmCN, ""},
	}
	for _, tc := range tests {
		realm, bare := resolveModelRealm(tc.in)
		if realm != tc.realm || bare != tc.bare {
			t.Errorf("resolveModelRealm(%q) = (%q, %q), want (%q, %q)",
				tc.in, realm, bare, tc.realm, tc.bare)
		}
	}
}

func TestWorkbuddyRealmQualified(t *testing.T) {
	qualified := []string{"cn:glm-5.2", "global:glm-5.2", "  global:glm-5.2  ", "cn:"}
	for _, in := range qualified {
		if !realmQualified(in) {
			t.Errorf("realmQualified(%q) = false, want true", in)
		}
	}
	bare := []string{"glm-5.2", "cn", "global", "", "   ", ":", "intl:glm-5.2", "CN:glm-5.2"}
	for _, in := range bare {
		if realmQualified(in) {
			t.Errorf("realmQualified(%q) = true, want false", in)
		}
	}
}

func TestWorkbuddyQualifyModelID(t *testing.T) {
	tests := []struct{ realm, id, want string }{
		{realmCN, "glm-5.2", "cn:glm-5.2"},
		{realmGlobal, "glm-5.2", "global:glm-5.2"},
		{"", "glm-5.2", "cn:glm-5.2"},
	}
	for _, tc := range tests {
		if got := qualifyModelID(tc.realm, tc.id); got != tc.want {
			t.Errorf("qualifyModelID(%q, %q) = %q, want %q", tc.realm, tc.id, got, tc.want)
		}
	}
}

// TestWorkbuddyQualifyThenResolveRoundTrips pins the publisher and the parser
// together.  The catalogue publishes qualifyModelID and the chat path parses it
// back with resolveModelRealm, so a drift between the two is a model nobody can
// call -- a failure that would otherwise only surface as a vendor 11102.
func TestWorkbuddyQualifyThenResolveRoundTrips(t *testing.T) {
	for _, realm := range []string{realmCN, realmGlobal} {
		for _, id := range []string{"glm-5.2", "deepseek-v4-pro", "claude-sonnet-4"} {
			gotRealm, gotBare := resolveModelRealm(qualifyModelID(realm, id))
			if gotRealm != realm || gotBare != id {
				t.Errorf("round trip of (%q, %q) = (%q, %q), want it back unchanged",
					realm, id, gotRealm, gotBare)
			}
		}
	}
}

func TestWorkbuddyRouteModelPinsAnExplicitRealm(t *testing.T) {
	for _, realm := range []string{realmCN, realmGlobal} {
		r := routeModel(realm + ":glm-5.2")
		if r.bare != "glm-5.2" {
			t.Errorf("%s: bare = %q, want glm-5.2", realm, r.bare)
		}
		if len(r.realms) != 1 || r.realms[0] != realm {
			t.Errorf("%s: realms = %v, want exactly [%s]", realm, r.realms, realm)
		}
		if r.affinityRealm != realm {
			t.Errorf("%s: affinityRealm = %q, want %q", realm, r.affinityRealm, realm)
		}
	}
}

// TestWorkbuddyRouteModelTreatsABareNameAsAPreference is the documented
// divergence from the reference.  The reference hard-defaults a bare name to
// the domestic realm, which is right for its single-tenant deployment; here a
// bare name prefers domestic but may fall back, because an operator running
// only an international account must not lose every bare-name request.
func TestWorkbuddyRouteModelTreatsABareNameAsAPreference(t *testing.T) {
	r := routeModel("glm-5.2")
	if r.bare != "glm-5.2" {
		t.Fatalf("bare = %q, want glm-5.2", r.bare)
	}
	if len(r.realms) != 2 || r.realms[0] != realmCN || r.realms[1] != "" {
		t.Fatalf("realms = %v, want [cn \"\"]", r.realms)
	}
	if r.affinityRealm != "" {
		t.Fatalf("affinityRealm = %q; a bare name must not pin the realm, or a "+
			"conversation bound to an international account would be dropped", r.affinityRealm)
	}
}

// --- pool realm filtering ---------------------------------------------------

// realmAuths is one credential per realm.  A bare Auth value defaults to the
// domestic realm, so the realm has to be set explicitly here.
func realmAuths() (*Auth, *Auth) {
	cn := &Auth{AccessToken: "at-cn-0000000000", UID: "u-cn", Realm: realmCN, Domain: "copilot.tencent.com"}
	intl := &Auth{AccessToken: "at-intl-00000000", UID: "u-intl", Realm: realmGlobal, Domain: "workbuddy.ai"}
	return cn, intl
}

func TestWorkbuddyPoolRealmsListsTheDomesticRealmFirst(t *testing.T) {
	cn, intl := realmAuths()
	// Deliberately international-first: the order must come from the code, not
	// from how the credentials happen to be stored on disk.
	p, _ := newPickPool([]*Auth{intl, cn})
	got := p.Realms()
	if len(got) != 2 || got[0] != realmCN || got[1] != realmGlobal {
		t.Fatalf("Realms() = %v, want [%s %s]", got, realmCN, realmGlobal)
	}
}

func TestWorkbuddyPoolRealmsDeduplicates(t *testing.T) {
	cn, intl := realmAuths()
	second := &Auth{AccessToken: "at-cn2-000000000", UID: "u-cn2", Realm: realmCN}
	p, _ := newPickPool([]*Auth{cn, second, intl})
	if got := p.Realms(); len(got) != 2 {
		t.Fatalf("Realms() = %v, want one entry per realm", got)
	}
}

func TestWorkbuddyPoolRealmsIsEmptyWithoutAccounts(t *testing.T) {
	p, _ := newPickPool(nil)
	if got := p.Realms(); len(got) != 0 {
		t.Fatalf("Realms() = %v, want none", got)
	}
}

// TestWorkbuddyPoolPickForModelInRealmRefusesTheWrongRealm is the safety
// property the filter exists for: a domestic-only model name sent to the
// international catalogue answers 11102, so the picker must not offer a
// credential from the other realm at all.
func TestWorkbuddyPoolPickForModelInRealmRefusesTheWrongRealm(t *testing.T) {
	cn, _ := realmAuths()
	p, _ := newPickPool([]*Auth{cn})

	if a, ok := p.PickForModelInRealm(nil, "", realmGlobal); ok {
		t.Fatalf("picked %s for the global realm; only a domestic credential exists", a.ID())
	}
	a, ok := p.PickForModelInRealm(nil, "", realmCN)
	if !ok || a.ID() != cn.ID() {
		t.Fatalf("domestic pick = %v/%v, want %s", a, ok, cn.ID())
	}
}

func TestWorkbuddyPoolPickForModelInRealmStaysInsideTheRealm(t *testing.T) {
	cn, intl := realmAuths()
	p, clk := newPickPool([]*Auth{cn, intl})
	// Repeated picks must keep landing on the international credential even
	// though the round-robin cursor walks past the domestic one every time.
	for i := 0; i < 6; i++ {
		a, ok := p.PickForModelInRealm(nil, "", realmGlobal)
		if !ok || a.ID() != intl.ID() {
			t.Fatalf("global pick %d = %v/%v, want %s", i, a, ok, intl.ID())
		}
		clk.advance(time.Second)
	}
}

func TestWorkbuddyPoolUsableForModelInRealm(t *testing.T) {
	cn, intl := realmAuths()
	p, _ := newPickPool([]*Auth{cn, intl})

	if !p.UsableForModelInRealm(cn.ID(), "", realmCN) {
		t.Error("the domestic credential must be usable in the domestic realm")
	}
	if p.UsableForModelInRealm(cn.ID(), "", realmGlobal) {
		t.Error("the domestic credential must not be usable in the international realm")
	}
	if !p.UsableForModelInRealm(intl.ID(), "", realmGlobal) {
		t.Error("the international credential must be usable in the international realm")
	}
	if p.UsableForModelInRealm(intl.ID(), "", realmCN) {
		t.Error("the international credential must not be usable in the domestic realm")
	}
}

// TestWorkbuddyPoolAnEmptyRealmKeepsEveryAccountEligible pins the opt-out.  The
// realm filter has to be invisible to the callers that never name a realm, or
// the split would change behaviour for every existing install.
func TestWorkbuddyPoolAnEmptyRealmKeepsEveryAccountEligible(t *testing.T) {
	cn, intl := realmAuths()
	p, _ := newPickPool([]*Auth{cn, intl})
	if !p.UsableForModelInRealm(cn.ID(), "", "") {
		t.Error("an empty realm must not exclude the domestic credential")
	}
	if !p.UsableForModelInRealm(intl.ID(), "", "") {
		t.Error("an empty realm must not exclude the international credential")
	}
	if a, ok := p.PickForModelInRealm(nil, "", ""); !ok || a == nil {
		t.Error("an empty realm and an empty model must still pick an account")
	}
}

// --- the catalogue ----------------------------------------------------------

// TestWorkbuddyCatalogueListsBothRealms is the defect this change fixes.  Models
// used to pick ONE account and publish whatever catalogue that account's realm
// answered, so an install holding both a domestic and an international
// credential saw only one of the two catalogues -- and which one it saw
// depended on round-robin luck.
func TestWorkbuddyCatalogueListsBothRealms(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		// The fake answers with the host it was asked for, so the test can tell
		// which realm produced which entry without hardcoding vendor hosts.
		ids := []string{req.URL.Hostname()}
		if req.URL.Path == v3ConfigPath {
			return jsonResponse(200, v3ConfigBody(ids...)), nil
		}
		return jsonResponse(200, globalEnterpriseBody(ids...)), nil
	}}
	files := cnAccountFiles()
	files[intlCreds] = intlAccountFiles()[intlCreds]
	c, _ := panelClient(t, rt, files)

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	byRealm := map[string]string{}
	for _, m := range models {
		if !realmQualified(m.ID) {
			t.Errorf("catalogue id %q is not realm-qualified", m.ID)
			continue
		}
		realm, bare := resolveModelRealm(m.ID)
		byRealm[realm] = bare
		if got := m.Extra["realm"]; got != realm {
			t.Errorf("%s carries extra[realm] = %v, want %q", m.ID, got, realm)
		}
	}
	if len(byRealm) != 2 {
		t.Fatalf("the catalogue covered %d realm(s) %v; want both", len(byRealm), byRealm)
	}
	if byRealm[realmCN] == "" || byRealm[realmGlobal] == "" {
		t.Fatalf("catalogue realms = %v, want a domestic and an international entry", byRealm)
	}
	if byRealm[realmCN] == byRealm[realmGlobal] {
		t.Fatalf("both realms answered for the same host %q; the fixture did not separate them",
			byRealm[realmCN])
	}
}

// --- the chat path ----------------------------------------------------------

// realmSSE is the smallest upstream stream the aggregator accepts.
const realmSSE = `data: {"code":0,"msg":"","data":{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"glm-5.2","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}}

data: [DONE]

`

// TestWorkbuddyChatRefusesARealmNoAccountServes is the hard half of the
// protocol: an explicit prefix is a promise, so a domestic-only install must
// fail a "global:" request rather than quietly serve it from the other
// catalogue, where the vendor would answer 11102.
func TestWorkbuddyChatRefusesARealmNoAccountServes(t *testing.T) {
	rt := &fakeRT{}
	c, _ := panelClient(t, rt, cnAccountFiles())

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "global:glm-5.2",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("a global-realm request was served by a domestic-only install")
	}
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want core.ErrNotConfigured", err)
	}
	if len(rt.calls) != 0 {
		t.Fatalf("the refused request still made %d upstream call(s): %v", len(rt.calls), wbPaths(rt))
	}
}

// TestWorkbuddyChatRejectsARealmWithNoModelName covers the degenerate prefix:
// "global:" names a realm and no model, which is not a request the vendor can
// answer.
func TestWorkbuddyChatRejectsARealmWithNoModelName(t *testing.T) {
	c, _ := panelClient(t, &fakeRT{}, cnAccountFiles())
	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "global:",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want core.ErrUnsupported", err)
	}
}

// TestWorkbuddyChatStripsTheRealmPrefixFromTheWireBody: the realm is this
// gateway's addressing scheme, so the vendor must never see it.  A leak here is
// a 11102 "model not found" for a model that plainly exists.
func TestWorkbuddyChatStripsTheRealmPrefixFromTheWireBody(t *testing.T) {
	var (
		mu   sync.Mutex
		wire string
	)
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/chat/completions") {
			raw, _ := io.ReadAll(req.Body)
			mu.Lock()
			wire = string(raw)
			mu.Unlock()
			return sseResponse(200, realmSSE, nil), nil
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	}}
	files := cnAccountFiles()
	files[intlCreds] = intlAccountFiles()[intlCreds]
	c, _ := panelClient(t, rt, files)

	st, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "global:glm-5.2",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer func() { _ = st.Close() }()

	mu.Lock()
	got := wire
	mu.Unlock()
	if got == "" {
		t.Fatal("no chat request reached the fake transport")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("the wire body is not JSON: %v", err)
	}
	if payload["model"] != "glm-5.2" {
		t.Fatalf("wire model = %v, want the bare glm-5.2 (the realm prefix is gateway-only)",
			payload["model"])
	}
}

// TestWorkbuddyChatNamesTheServedAccount pins the gateway-facing attribution.
// A success used to carry no account at all, so the usage ledger filed every
// successful turn under "(unrouted)" and the console row showed a blank.  The
// account that served the turn is known here and nowhere else, and with two
// realms in play the test also proves the realm filter -- not merely the first
// account in the pool -- decided the answer.
func TestWorkbuddyChatNamesTheServedAccount(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"cn:glm-5.2", "uid-cn-0001"},
		{"global:glm-5.2", "uid-intl-0001"},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/chat/completions") {
					return sseResponse(200, realmSSE, nil), nil
				}
				return jsonResponse(http.StatusNotFound, `{}`), nil
			}}
			files := cnAccountFiles()
			files[intlCreds] = intlAccountFiles()[intlCreds]
			c, _ := panelClient(t, rt, files)

			var served string
			st, err := c.Chat(context.Background(), &core.ChatRequest{
				Model:    tc.model,
				Messages: []core.Message{{Role: "user", Content: "hi"}},
				ServedBy: &served,
			})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			defer func() { _ = st.Close() }()

			if served != tc.want {
				t.Errorf("ServedBy = %q, want %q", served, tc.want)
			}
		})
	}
}

// TestWorkbuddyChatKeepsTheAccountLeaseOnTheCallerRequest is the regression
// guard for a leaked gateway slot.  Chat used to shallow-copy the request to
// strip the realm prefix; the copy carried the gateway's unexported per-account
// lease, so AcquireAccountSlot filled the copy while the gateway released the
// original.  Every success leaked one slot, and after max_in_flight_per_account
// successes the account answered 429 forever even with nothing in flight.
func TestWorkbuddyChatKeepsTheAccountLeaseOnTheCallerRequest(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/chat/completions") {
			return sseResponse(200, realmSSE, nil), nil
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	var released []string
	req := &core.ChatRequest{
		Model:    "cn:glm-5.2",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}
	req.SetAccountAcquirer(func(accountID string) (func(), error) {
		return func() { released = append(released, accountID) }, nil
	})

	st, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer func() { _ = st.Close() }()

	// The gateway releases through the request it passed in.  If Chat took the
	// slot on a copy, this releases nothing and the slot leaks.
	req.ReleaseAccountSlot()
	if len(released) != 1 {
		t.Fatalf("released = %v, want exactly the account Chat leased", released)
	}
}
