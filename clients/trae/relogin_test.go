package trae

import (
	"context"
	"testing"
)

// The e-mail a discovered credential carries is the one piece of identity the
// vendor hands over for free, so it has to reach the panel.  A panel web login
// only ever gets a nickname, which is why the operator's own note exists.
func TestAccountsSurfaceTheDiscoveredEmail(t *testing.T) {
	c := panelClient(t, nil, nil)
	c.pool.Replace([]*Auth{{
		AccessToken: "tok",
		UserID:      "u-mail",
		Username:    "nick",
		Email:       "ops@example.com",
	}})

	list, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	for _, rec := range list {
		if rec.ID != "u-mail" {
			continue
		}
		if rec.Fields["email"] != "ops@example.com" {
			t.Errorf("fields.email = %v, want ops@example.com", rec.Fields["email"])
		}
		return
	}
	t.Fatalf("account u-mail is missing from Accounts(): %+v", list)
}

// relogin_test.go pins trae's half of the panel's 「重登」 button.
//
// 面板按 fields.relogin 判断一份凭据是不是已经死了。这里钉住两件事：判据只认
// 「上游把凭据拒了」（1001/4010 与 401），不把配额、限流这种等一等就好的状态
// 也算进去；以及这个字段真的出现在 Accounts() 给出的记录上。

// TestCredentialDeadIsNarrowerThanEveryCooldown pins which pool verdicts raise
// the button.  Flagging a quota cooldown would put 「重登」 in front of a healthy
// credential, and an operator re-logging in to fix a rate limit changes nothing.
func TestCredentialDeadIsNarrowerThanEveryCooldown(t *testing.T) {
	cases := []struct {
		name  string
		state string
		note  string
		want  bool
	}{
		{"401 parks the account invalid", stateInvalid, "session_dead", true},
		{"401 with no usable refresh token", stateInvalid, "auth", true},
		{"1001 rejects the credential", stateCooling, "auth", true},
		{"quota is not a credential problem", stateExhausted, "quota", false},
		{"plan limit is not a credential problem", stateExhausted, "plan_limit", false},
		{"rate limit is not a credential problem", stateCooling, "soft_rate", false},
		{"a healthy account is not flagged", stateReady, "", false},
	}
	for _, tc := range cases {
		if got := credentialDead(tc.state, tc.note); got != tc.want {
			t.Errorf("%s: credentialDead(%q, %q) = %v, want %v",
				tc.name, tc.state, tc.note, got, tc.want)
		}
	}
}

// TestAccountsFlagDeadCredentialsForRelogin is the end-to-end half: once the
// pool has parked a credential because the upstream rejected it, the record the
// panel serves has to carry fields.relogin, which is what renders the button.
func TestAccountsFlagDeadCredentialsForRelogin(t *testing.T) {
	c := panelClient(t, nil, nil)
	addTestAccount(t, c, map[string]string{"refresh_token": "rt", "user_id": "u-relogin"})

	live := c.pool.Accounts()
	if len(live) == 0 {
		t.Fatal("the added account never reached the pool")
	}
	c.pool.MarkFailure(live[0], &Error{Kind: ErrAuth, Msg: "credential rejected"})

	list, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	id := live[0].ID()
	for _, rec := range list {
		if rec.ID != id {
			continue
		}
		if rec.Fields["relogin"] != true {
			t.Errorf("fields.relogin = %v, want true after a 1001 rejection", rec.Fields["relogin"])
		}
		return
	}
	t.Fatalf("account %s is missing from Accounts()", id)
}

// A quota cooldown is not a reason to re-login, so the same end-to-end path must
// NOT raise the flag for it.
func TestAccountsDoNotFlagQuotaForRelogin(t *testing.T) {
	c := panelClient(t, nil, nil)
	addTestAccount(t, c, map[string]string{"refresh_token": "rt", "user_id": "u-quota"})

	live := c.pool.Accounts()
	if len(live) == 0 {
		t.Fatal("the added account never reached the pool")
	}
	c.pool.MarkFailure(live[0], &Error{Kind: ErrQuota, Msg: "out of credits"})

	list, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	id := live[0].ID()
	for _, rec := range list {
		if rec.ID != id {
			continue
		}
		if _, ok := rec.Fields["relogin"]; ok {
			t.Errorf("fields.relogin present for a quota cooldown: %v", rec.Fields["relogin"])
		}
		return
	}
	t.Fatalf("account %s is missing from Accounts()", id)
}
