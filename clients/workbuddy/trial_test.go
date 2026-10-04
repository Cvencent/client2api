package workbuddy

import (
	"context"
	"net/http"
	"testing"
)

// The trial endpoint exists on the international realm only.  A CN account must
// get a Go error and, crucially, no network call at all.
func TestWorkbuddyClaimTrialRefusesTheCNRealmWithoutARequest(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		t.Fatalf("a CN account reached %s %s; the trial endpoint is global-only",
			req.Method, req.URL.Path)
		return nil, nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	claimed, err := c.ClaimTrial(context.Background(), wbCNAuth(t, c))
	if err == nil {
		t.Fatalf("ClaimTrial on CN returned claimed=%v and no error", claimed)
	}
	if claimed {
		t.Fatalf("claimed = true on the CN realm")
	}
	if got, want := err.Error(), "claim trial: only global accounts"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if n := len(wbPaths(rt)); n != 0 {
		t.Fatalf("%d requests were sent, want 0", n)
	}
}

func TestWorkbuddyClaimTrialOnTheGlobalRealm(t *testing.T) {
	t.Run("first claim", func(t *testing.T) {
		var seen *http.Request
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			seen = req
			return jsonResponse(200, wbEnvelope(`{"credit":500}`)), nil
		}}
		c, _ := panelClient(t, rt, intlAccountFiles())
		a := wbIntlAuth(t, c)

		claimed, err := c.ClaimTrial(context.Background(), a)
		if err != nil {
			t.Fatalf("ClaimTrial: %v", err)
		}
		if !claimed {
			t.Fatal("claimed = false on a successful first claim")
		}
		req := wbIdentity(t, seen, a)
		if req.Method != http.MethodPost || req.URL.Path != trialPath {
			t.Fatalf("%s %s, want POST %s", req.Method, req.URL.Path, trialPath)
		}
		if got := req.URL.Host; got != "www.workbuddy.ai" && got != "workbuddy.ai" {
			t.Fatalf("host = %q, want the global billing base", got)
		}
	})

	// Two fingerprints of "already claimed" mean an idempotent success, not a
	// failure: the envelope form (`code=14051`) and the raw-body form
	// (`"code":14051`) that arrives with an HTTP error.
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"envelope refusal", http.StatusOK, wbRefusal(14051, "the trial has already been claimed")},
		{"raw body refusal", http.StatusBadRequest, `{"code":14051,"msg":"already claimed"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
				return jsonResponse(tc.status, tc.body), nil
			}}
			c, _ := panelClient(t, rt, intlAccountFiles())

			claimed, err := c.ClaimTrial(context.Background(), wbIntlAuth(t, c))
			if err != nil {
				t.Fatalf("ClaimTrial: %v, want the idempotent success path", err)
			}
			if claimed {
				t.Fatal("claimed = true for an already-claimed trial")
			}
		})
	}

	// Any other refusal is a real failure and must keep its error.
	t.Run("other refusal", func(t *testing.T) {
		rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusForbidden, `{"code":14004,"msg":"the trial is not open to this account"}`), nil
		}}
		c, _ := panelClient(t, rt, intlAccountFiles())

		claimed, err := c.ClaimTrial(context.Background(), wbIntlAuth(t, c))
		if err == nil {
			t.Fatalf("ClaimTrial returned claimed=%v and no error for a real refusal", claimed)
		}
		if claimed {
			t.Fatal("claimed = true for a real refusal")
		}
	})
}

func TestWorkbuddyTrialAlreadyErrMatchesBothSpellings(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"code=14051 msg=already claimed", true},
		{`{"code":14051,"msg":"already claimed"}`, true},
		{"CODE=14051", true},
		{"code=1405", false},
		{"code=14052 msg=nope", false},
		{"", false},
		{"the trial has already been claimed", false},
	}
	for _, tc := range tests {
		if got := trialAlreadyErr(tc.msg); got != tc.want {
			t.Errorf("trialAlreadyErr(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}
