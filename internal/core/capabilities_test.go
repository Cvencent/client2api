package core

import (
	"context"
	"testing"
)

// plainClient satisfies Client and nothing else -- the shape a third-party
// module has before it opts into any of the shared machinery.
type plainClient struct{ name string }

func (c *plainClient) Name() string                            { return c.name }
func (c *plainClient) Models(context.Context) ([]Model, error) { return nil, nil }
func (c *plainClient) Chat(context.Context, *ChatRequest) (Stream, error) {
	return nil, nil
}
func (c *plainClient) Status(context.Context) Status {
	return Status{Name: c.name, Ready: true}
}

type degradingClient struct{ plainClient }

func (c *degradingClient) Degrade(*ChatRequest) bool { return true }

type planningClient struct{ plainClient }

func (c *planningClient) Batches() []Batch {
	return []Batch{{Name: "checkin", Codes: []string{"checkin"}}}
}

type healthyClient struct{ plainClient }

func (c *healthyClient) Health() Health { return Health{Servable: true} }

type liveClient struct{ plainClient }

func (c *liveClient) ApplyLive(LiveSettings) {}

// checkinStub answers CheckinActions from a field so a test can model both
// shapes a real module produces: an action on offer right now, and a module
// that has check-in but nothing to offer because it holds no usable account.
type checkinStub struct {
	plainClient
	actions []CheckinAction
}

func (c *checkinStub) CheckinActions(context.Context) []CheckinAction { return c.actions }
func (c *checkinStub) Checkin(context.Context, string, string) (CheckinResult, error) {
	return CheckinResult{OK: true}, nil
}

// TestCapabilitiesOfReportsTheOptInHalf pins the four flags that tell the panel
// whether a module has opted into the shared machinery.  They are the only
// static evidence that the failover, degrade, scheduling, health and live
// settings code is reachable for a given module -- without them an
// unimplemented capability is indistinguishable from a broken one.
func TestCapabilitiesOfReportsTheOptInHalf(t *testing.T) {
	cases := []struct {
		name    string
		client  Client
		degrade bool
		batches bool
		health  bool
		live    bool
	}{
		{
			name:   "a module that opted into nothing",
			client: &plainClient{name: "plain"},
		},
		{
			name:    "a module with the degraded retry",
			client:  &degradingClient{plainClient{name: "dg"}},
			degrade: true,
		},
		{
			name:    "a module that can be scheduled",
			client:  &planningClient{plainClient{name: "pl"}},
			batches: true,
		},
		{
			name:   "a module that reports health",
			client: &healthyClient{plainClient{name: "hl"}},
			health: true,
		},
		{
			name:   "a module that takes live settings",
			client: &liveClient{plainClient{name: "lv"}},
			live:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CapabilitiesOf(context.Background(), tc.client)
			if got.Degrade != tc.degrade {
				t.Errorf("Degrade = %v, want %v", got.Degrade, tc.degrade)
			}
			if got.Batches != tc.batches {
				t.Errorf("Batches = %v, want %v", got.Batches, tc.batches)
			}
			if got.Health != tc.health {
				t.Errorf("Health = %v, want %v", got.Health, tc.health)
			}
			if got.Live != tc.live {
				t.Errorf("Live = %v, want %v", got.Live, tc.live)
			}
		})
	}
}

// TestTheOptInFlagsDoNotImplyEachOther is the point of four separate flags: a
// module can be schedulable without being degradable, or take live settings
// without reporting health.  Folding them into one "advanced" boolean would
// hide which half is actually missing.
func TestTheOptInFlagsDoNotImplyEachOther(t *testing.T) {
	got := CapabilitiesOf(context.Background(), &degradingClient{plainClient{name: "dg"}})
	if !got.Degrade {
		t.Fatalf("Degrade = false, want true")
	}
	for _, f := range []struct {
		name string
		got  bool
	}{
		{"Batches", got.Batches},
		{"Health", got.Health},
		{"Live", got.Live},
	} {
		if f.got {
			t.Errorf("%s = true, want false: one capability must not imply another", f.name)
		}
	}
}

// TestCapabilitiesOnANonOptedInModuleStayZero is a guard on the type
// assertions: a wrong interface name would compile and silently report false
// for every module, which is exactly the failure mode this reporting exists to
// expose.
func TestCapabilitiesOnANonOptedInModuleStayZero(t *testing.T) {
	got := CapabilitiesOf(context.Background(), &plainClient{name: "plain"})
	if got.Manage || got.Import || got.Login || got.Checkin || got.Refresh || got.Tasks {
		t.Errorf("a bare Client reported an account/task capability: %+v", got)
	}
	if got.Degrade || got.Batches || got.Health || got.Live {
		t.Errorf("a bare Client reported an opt-in capability: %+v", got)
	}
}

// plainLoginClient is a module with a single login flow -- the shape every
// module had before the realm picker existed.
type plainLoginClient struct{ plainClient }

func (c *plainLoginClient) StartLogin(context.Context) (LoginState, error) {
	return LoginState{State: LoginPending}, nil
}

func (c *plainLoginClient) PollLogin(context.Context, string) (LoginState, error) {
	return LoginState{}, nil
}

func (c *plainLoginClient) CancelLogin(context.Context, string) error { return nil }

// realmClient serves two upstream services, so "add an account" is a choice.
type realmClient struct{ plainClient }

func (c *realmClient) StartLogin(context.Context) (LoginState, error) {
	return LoginState{State: LoginPending}, nil
}

func (c *realmClient) PollLogin(context.Context, string) (LoginState, error) {
	return LoginState{}, nil
}

func (c *realmClient) CancelLogin(context.Context, string) error { return nil }

func (c *realmClient) LoginRealms(context.Context) []LoginRealm {
	return []LoginRealm{
		{Code: "cn", Name: "国内版"},
		{Code: "global", Name: "国际版"},
	}
}

func (c *realmClient) StartLoginRealm(_ context.Context, realm string) (LoginState, error) {
	return LoginState{State: LoginPending, Realm: realm}, nil
}

// TestARealmAwareLoginReportsItsRealms pins the two facts the panel needs to
// decide whether to render the picker at all: that the module has a login, and
// which realms it can add an account to.  A single-realm module must report the
// login flag with an EMPTY realm list -- that is what tells the panel to show no
// picker rather than an empty one, and it is the same "no control for a
// mechanism that does not exist" rule the rest of the panel follows.
func TestARealmAwareLoginReportsItsRealms(t *testing.T) {
	got := CapabilitiesOf(context.Background(), &realmClient{plainClient{name: "rw"}})
	if !got.Login {
		t.Fatalf("Login = false, want true for a realm-aware module")
	}
	if len(got.Realms) != 2 {
		t.Fatalf("Realms = %+v, want exactly two", got.Realms)
	}
	if got.Realms[0].Code != "cn" || got.Realms[1].Code != "global" {
		t.Errorf("Realms = %+v, want cn then global in the module's own order", got.Realms)
	}
	if got.Realms[0].Name == "" || got.Realms[1].Name == "" {
		t.Errorf("Realms = %+v, want a human label on each", got.Realms)
	}

	plain := CapabilitiesOf(context.Background(), &plainLoginClient{plainClient{name: "pl"}})
	if !plain.Login {
		t.Fatalf("Login = false for a plain LoginProvider, want true")
	}
	if len(plain.Realms) != 0 {
		t.Errorf("a single-realm login advertised realms %+v, want none", plain.Realms)
	}
}

// TestAsRealmLoginProviderRefusesAPlainLogin is the distinction the panel acts
// on.  Driving a plain LoginProvider through the realm entry point would accept
// the operator's choice and silently ignore it, landing the credential on the
// configured default realm -- the one outcome worse than refusing.
func TestAsRealmLoginProviderRefusesAPlainLogin(t *testing.T) {
	if _, ok := AsRealmLoginProvider(&plainLoginClient{plainClient{name: "pl"}}); ok {
		t.Errorf("AsRealmLoginProvider accepted a module with no realm support")
	}
	rp, ok := AsRealmLoginProvider(&realmClient{plainClient{name: "rw"}})
	if !ok {
		t.Fatalf("AsRealmLoginProvider refused a realm-aware module")
	}
	st, err := rp.StartLoginRealm(context.Background(), "global")
	if err != nil {
		t.Fatalf("StartLoginRealm: %v", err)
	}
	if st.Realm != "global" {
		t.Errorf("Realm = %q, want global carried back on the state", st.Realm)
	}
}

// TestCheckinAndCheckinReadyAnswerDifferentQuestions pins the split between the
// two check-in bits, which exist because CheckinActions is allowed to narrow
// itself to the accounts that exist at this instant.  codearts answers no
// actions while it holds no credential and minimaxcode while the credential is
// expired; both modules DO have check-in, so a client-level "每日签到" column
// reading Checkin must keep saying yes, while the per-account button reading
// CheckinReady must stay hidden.  Collapsing the two is exactly the defect this
// test prevents: the panel told the operator that two modules had no check-in.
func TestCheckinAndCheckinReadyAnswerDifferentQuestions(t *testing.T) {
	cases := []struct {
		name     string
		provider bool
		actions  []CheckinAction
		checkin  bool
		ready    bool
	}{
		{
			name: "a module that does not implement CheckinProvider",
		},
		{
			name:     "a module with an action on offer",
			provider: true,
			actions:  []CheckinAction{{ID: "daily", Label: "daily gift"}},
			checkin:  true,
			ready:    true,
		},
		{
			name:     "a module that has check-in but no account to offer it for",
			provider: true,
			actions:  []CheckinAction{},
			checkin:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c Client = &plainClient{name: "n"}
			if tc.provider {
				c = &checkinStub{plainClient{name: "n"}, tc.actions}
			}
			got := CapabilitiesOf(context.Background(), c)
			if got.Checkin != tc.checkin {
				t.Errorf("Checkin = %v, want %v", got.Checkin, tc.checkin)
			}
			if got.CheckinReady != tc.ready {
				t.Errorf("CheckinReady = %v, want %v", got.CheckinReady, tc.ready)
			}
			if tc.ready && len(got.Actions) == 0 {
				t.Error("CheckinReady is true with no actions to render")
			}
			if !tc.ready && len(got.Actions) != 0 {
				t.Errorf("Actions = %+v, want none when CheckinReady is false", got.Actions)
			}
		})
	}
}
