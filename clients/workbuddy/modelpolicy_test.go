package workbuddy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// modelpolicy_test.go pins the per-account model policy and its account-level
// force override. These settings are operator decisions, so they live in the
// credential file and survive a restart instead of disappearing with runtime
// pool health.

func TestWorkbuddyAuthModelPolicyRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	a := &Auth{
		AccessToken:    "at-111111111111",
		UID:            "u1",
		Nickname:       "operator-main",
		FilePath:       path,
		DisabledModels: []string{"global:internal-model", "expensive-model"},
		ForceEnabled:   true,
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved credential: %v", err)
	}
	b, err := ParseAuth(raw)
	if err != nil {
		t.Fatalf("ParseAuth: %v", err)
	}
	if !b.ForceEnabled {
		t.Fatal("force_enabled did not survive the credential round trip")
	}
	if got := strings.Join(b.DisabledModels, ","); got != "expensive-model,internal-model" {
		t.Fatalf("disabled_models = %q, want normalised and sorted", got)
	}
}

func TestWorkbuddyAuthAcceptsFlatModelPolicy(t *testing.T) {
	raw := []byte(`{
		"accessToken":"at-111111111111",
		"uid":"u1",
		"disabled_models":["model-a"],
		"force_enabled":true
	}`)
	a, err := ParseAuth(raw)
	if err != nil {
		t.Fatalf("ParseAuth: %v", err)
	}
	if !a.ForceEnabled || len(a.DisabledModels) != 1 || a.DisabledModels[0] != "model-a" {
		t.Fatalf("flat policy parsed as force=%v disabled=%v", a.ForceEnabled, a.DisabledModels)
	}
}

func TestWorkbuddySetAccountModelPolicyPersistsAndReachesPool(t *testing.T) {
	c, _ := panelClient(t, nil, map[string]string{
		"wb-policy.json": `{"accessToken":"at-111111111111","uid":"u-policy","nickname":"policy"}`,
	})
	a := c.findAuth("u-policy")
	if a == nil {
		t.Fatal("fixture account was not loaded")
	}

	if err := c.SetAccountModelPolicy(context.Background(), a.ID(), []string{"locked-model", "locked-model", " "}, true); err != nil {
		t.Fatalf("SetAccountModelPolicy: %v", err)
	}
	if a.ModelAllowed("locked-model") {
		t.Fatal("a disabled model is still allowed")
	}
	if !a.ModelAllowed("free-model") {
		t.Fatal("a model outside the blacklist must stay allowed")
	}
	if got := a.DisabledModelsValue(); len(got) != 1 || got[0] != "locked-model" {
		t.Fatalf("normalised disabled_models = %v, want [locked-model]", got)
	}

	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatalf("read persisted credential: %v", err)
	}
	b, err := ParseAuth(raw)
	if err != nil {
		t.Fatalf("ParseAuth after policy save: %v", err)
	}
	if !b.ForceEnabled {
		t.Fatal("force_enabled was not persisted")
	}
	if got := b.DisabledModelsValue(); len(got) != 1 || got[0] != "locked-model" {
		t.Fatalf("persisted disabled_models = %v, want [locked-model]", got)
	}
}

func TestWorkbuddyModelPolicyBlocksOnlyTheDisabledModel(t *testing.T) {
	a, b := pickAuths()
	a.DisabledModels = []string{"model-x"}
	p, _ := newPickPool([]*Auth{a, b})

	if p.UsableForModel(a.ID(), "model-x") {
		t.Fatal("disabled model is still usable on account a")
	}
	if !p.UsableForModel(a.ID(), "model-y") {
		t.Fatal("disabling one model took the account out of rotation for other models")
	}
	got, ok := p.PickForModel(nil, "model-x")
	if !ok || got.ID() != b.ID() {
		t.Fatalf("PickForModel(model-x) = %v/%v, want the only account that allows it", got, ok)
	}
	if got, ok := p.PickForModel(nil, "model-y"); !ok || got == nil {
		t.Fatal("model-y has candidates and must still be pickable")
	}
}

// TestWorkbuddyForceEnabledFlipsAnyAccountParkToReady is the operator's actual
// intent behind 强制启用: a human has decided the account is usable, so the pool
// must both REPORT it as 可用 and actually ROUTE to it -- not merely relax one
// internal check while the panel keeps showing 冷却中/账号异常. It covers the
// account-level verdicts an operator reaches for the switch to clear.
func TestWorkbuddyForceEnabledFlipsAnyAccountParkToReady(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*Pool, *Auth)
	}{
		{
			name: "hard credit",
			apply: func(p *Pool, a *Auth) {
				p.MarkFailure(a, &Error{Kind: ErrHardCredit, Status: 402, Msg: "no credit"})
			},
		},
		{
			name: "low credit reserve",
			apply: func(p *Pool, a *Auth) {
				p.SetCreditsDetailed(a, 0, 100, 0, time.Time{}, 0)
			},
		},
		{
			name: "session dead",
			apply: func(p *Pool, a *Auth) {
				for i := 0; i < sessionDeadThreshold; i++ {
					p.MarkFailure(a, &Error{Kind: ErrSessionDead, Status: 401, Msg: "Offline user session not found"})
				}
			},
		},
		{
			name: "risk",
			apply: func(p *Pool, a *Auth) {
				p.MarkFailure(a, &Error{Kind: ErrAccountFault, Status: 403, Msg: "request illegal"})
			},
		},
		{
			name: "soft rate",
			apply: func(p *Pool, a *Auth) {
				p.MarkFailure(a, &Error{Kind: ErrSoftRate, Status: 429, RetryAfter: time.Hour, Msg: "slow down"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Control: without the switch the park is real.
			off := &Auth{AccessToken: "at-111111111111", UID: "u-off"}
			pOff, _ := newPickPool([]*Auth{off})
			tc.apply(pOff, off)
			if st := statusOf(t, pOff, off.ID()); st.State == stateReady {
				t.Fatalf("control: %s did not park the account without force", tc.name)
			}

			a := &Auth{AccessToken: "at-222222222222", UID: "u-on", ForceEnabled: true}
			p, _ := newPickPool([]*Auth{a})
			tc.apply(p, a)

			st := statusOf(t, p, a.ID())
			if st.State != stateReady {
				t.Fatalf("forced %s reported state %q, want %q (可用)", tc.name, st.State, stateReady)
			}
			if !st.Enabled {
				t.Fatalf("forced %s reported Enabled=false, want the account usable", tc.name)
			}
			if !p.UsableForModel(a.ID(), "free-model") {
				t.Fatalf("forced %s still unusable for free-model", tc.name)
			}
			got, ok := p.PickForModel(nil, "free-model")
			if !ok || got == nil || got.ID() != a.ID() {
				t.Fatalf("forced %s was not picked; PickForModel = %v/%v", tc.name, got, ok)
			}
		})
	}
}

// TestWorkbuddyForceEnabledStillHonorsPerModelParks pins the one line the
// operator switch does NOT cross: a per-model park (11102 unsupported, 6004
// model rate limit) is a fact about one model, not account health, so forcing
// the account must not hand it back for that exact model.
func TestWorkbuddyForceEnabledStillHonorsPerModelParks(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u1", ForceEnabled: true}
	p, _ := newPickPool([]*Auth{a})
	p.MarkModelBlocked(a, "model-x", ModelBlockReason)

	if p.UsableForModel(a.ID(), "model-x") {
		t.Fatal("a per-model park was overridden by force_enabled")
	}
	if !p.UsableForModel(a.ID(), "model-y") {
		t.Fatal("force_enabled must not park the account for other models")
	}
}
func TestWorkbuddyForceEnabledSurvivesCreditRefresh(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u1", ForceEnabled: true}
	p := reserveTestPool(t, a)
	p.SetCreditsDetailed(a, 0, 100, 0, time.Time{}, 0)
	if !p.UsableForModel(a.ID(), "free-model") {
		t.Fatal("a force-enabled account must stay usable after a zero-balance refresh")
	}
}

// TestWorkbuddyForceEnableViaPolicyClearsAccountParkButKeepsModelPark pins the
// operator's distinction: 强制启用 turns the account-level 冷却中/账号异常 into
// 可用, but it must not spend a per-model park that belongs to one model only.
func TestWorkbuddyForceEnableViaPolicyClearsAccountParkButKeepsModelPark(t *testing.T) {
	c, _ := panelClient(t, nil, map[string]string{
		"wb-policy.json": `{"accessToken":"at-111111111111","uid":"u-policy","nickname":"policy"}`,
	})
	a := c.findAuth("u-policy")
	if a == nil {
		t.Fatal("fixture account was not loaded")
	}
	c.pool.MarkFailure(a, &Error{Kind: ErrSoftRate, Status: 429, RetryAfter: time.Hour, Msg: "slow down"})
	if d := c.pool.MarkModelBlocked(a, "model-x", ModelBlockReason); d <= 0 {
		t.Fatalf("MarkModelBlocked = %v, want a per-model park", d)
	}
	if st := statusOf(t, c.pool, a.ID()); st.State == stateReady {
		t.Fatalf("control: account reported %q before force enable", st.State)
	}

	if err := c.SetAccountModelPolicy(context.Background(), a.ID(), nil, true); err != nil {
		t.Fatalf("SetAccountModelPolicy: %v", err)
	}
	if st := statusOf(t, c.pool, a.ID()); st.State != stateReady {
		t.Fatalf("forced account reported %q, want %q (可用)", st.State, stateReady)
	}
	if !c.pool.UsableForModel(a.ID(), "model-y") {
		t.Fatal("the forced account must be usable for other models")
	}
	if c.pool.UsableForModel(a.ID(), "model-x") {
		t.Fatal("force enable must not clear a per-model park")
	}
}
func TestWorkbuddyDisabledModelsAppearInAccountRecord(t *testing.T) {
	c, _ := panelClient(t, nil, map[string]string{
		"wb-policy.json": `{"accessToken":"at-111111111111","uid":"u-policy","disabled_models":["blocked-model"],"force_enabled":true}`,
	})
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	for _, rec := range recs {
		if rec.ID != "u-policy" {
			continue
		}
		got, ok := rec.Fields["disabled_models"].([]string)
		if !ok || len(got) != 1 || got[0] != "blocked-model" {
			t.Fatalf("disabled_models field = %#v, want [blocked-model]", rec.Fields["disabled_models"])
		}
		if rec.Fields["force_enabled"] != true {
			t.Fatalf("force_enabled field = %#v, want true", rec.Fields["force_enabled"])
		}
		return
	}
	t.Fatal("u-policy was not returned by Accounts")
}

// TestWorkbuddyUsableForModelIgnoresAccountsThatBlockTheModel pins the affinity
// predicate against a subtle tier bug: an account the operator disabled for
// this model must not count toward the "best priority tier" the predicate
// compares against. If it did, a higher-priority account blacklisted for the
// model would price every lower-priority account out of it, and a conversation
// bound to the allowed account would be answered "no account can serve this"
// even though one can.
func TestWorkbuddyUsableForModelIgnoresAccountsThatBlockTheModel(t *testing.T) {
	hi := &Auth{AccessToken: "at-111111111111", UID: "u-hi"}
	lo := &Auth{AccessToken: "at-222222222222", UID: "u-lo"}
	hi.DisabledModels = []string{"model-x"}
	core.SetAccountPriorities(map[string]map[string]int{
		"workbuddy": {hi.UID: -1, lo.UID: 0},
	})
	t.Cleanup(func() { core.SetAccountPriorities(nil) })

	p, _ := newPickPool([]*Auth{hi, lo})
	if p.UsableForModel(hi.ID(), "model-x") {
		t.Fatal("the account that disabled model-x must not be usable for it")
	}
	if !p.UsableForModel(lo.ID(), "model-x") {
		t.Fatal("a lower-priority account that allows model-x is still the best candidate for it")
	}
}
