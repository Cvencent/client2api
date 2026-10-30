package openaicompat

import (
	"testing"

	"client2api/internal/core"
)

// A 403 is three different answers on a relay, and only one of them is about
// the credential.  The live log is what these cases come from: chatanywhere
// answering "free points insufficient" and lingxiai answering "no permission to
// access group" both arrived as bare 403s, and the status-only branch marked
// those keys 账号异常 -- taking every model they serve out of rotation until an
// operator re-imported a key that had never expired.
func TestClassifyForbiddenIsNotAlwaysAuth(t *testing.T) {
	cases := []struct {
		name   string
		status int
		text   string
		want   failureKind
	}{
		{
			name:   "a spent free tier is a quota, not a dead key",
			status: 403,
			text:   "7天免费点数不足以支持本次请求，请等待当前额度窗口结束",
			want:   kindCredit,
		},
		{
			name:   "the English spelling of the same refusal",
			status: 403,
			text:   "The free points available in the current 7-day window are insufficient for this request.",
			want:   kindCredit,
		},
		{
			name:   "no permission for one group is a model refusal",
			status: 403,
			text:   "No permission to access group Gemini 反重力 (request id: abc)",
			want:   kindModelTurnedAway,
		},
		{
			name:   "a banned key stays terminal for the credential",
			status: 403,
			text:   "User has been banned",
			want:   kindAuth,
		},
		{
			name:   "an explicit invalid key stays auth",
			status: 403,
			text:   "invalid api key",
			want:   kindAuth,
		},
		{
			name:   "a bare 403 is ambiguous and keeps the account",
			status: 403,
			text:   "Forbidden",
			want:   kindForbidden,
		},
		{
			name:   "a 401 is still the credential whatever it says",
			status: 401,
			text:   "Forbidden",
			want:   kindAuth,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFailure(tc.status, tc.text); got != tc.want {
				t.Fatalf("classifyFailure(%d, %q) = %q, want %q", tc.status, tc.text, got, tc.want)
			}
		})
	}
}

// The core kinds decide what the pool and the gateway do, so an ambiguous 403
// must not become FailureAuth: that is the kind the pool answers by marking the
// credential invalid.
func TestForbiddenCoreKindRotatesWithoutKillingTheKey(t *testing.T) {
	if got := coreKindFor(kindForbidden); got != core.FailureModelRefused {
		t.Fatalf("coreKindFor(kindForbidden) = %q, want %q", got, core.FailureModelRefused)
	}
	if got := coreKindFor(kindAuth); got != core.FailureAuth {
		t.Fatalf("coreKindFor(kindAuth) = %q, want %q", got, core.FailureAuth)
	}
	// The spent-free-tier refusal parks on the quota path, which a window reset
	// or a top-up clears by itself.
	if got := coreKindFor(kindCredit); got != core.FailureQuota {
		t.Fatalf("coreKindFor(kindCredit) = %q, want %q", got, core.FailureQuota)
	}
}
