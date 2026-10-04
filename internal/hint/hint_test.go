package hint

import (
	"strings"
	"testing"

	"client2api/internal/core"
)

// The table only earns its place if the codes operators actually see resolve to
// the advice written for them.  Each case below is a real failure shape: the
// message is what the module puts in front of the operator today.
func TestKnownVendorFailuresGetTheirAdvice(t *testing.T) {
	cases := []struct {
		name    string
		client  string
		kind    core.FailureKind
		message string
		want    string
	}{
		{
			name:    "workbuddy trial already claimed",
			client:  "workbuddy",
			kind:    core.FailureQuota,
			message: "[workbuddy] upstream said 14051 trial already claimed",
			want:    "already claimed its trial",
		},
		{
			name:    "workbuddy global registration needed",
			client:  "workbuddy",
			message: "[workbuddy] trial not activated",
			want:    "global registration flow",
		},
		{
			name:    "workbuddy model not served by this account",
			client:  "workbuddy",
			message: "[workbuddy] code 11102 from /v2/chat/completions",
			want:    "account/model pair is",
		},
		{
			name:    "trae plan limit is not a failover case",
			client:  "trae",
			kind:    core.FailureQuota,
			message: "[trae] upstream error 1005",
			want:    "failover is deliberately disabled",
		},
		{
			name:    "trae credits exhausted",
			client:  "trae",
			kind:    core.FailureQuota,
			message: "[trae] llm_utils_chat returned 1001",
			want:    "credits are exhausted",
		},
		{
			name:    "zcode risk control",
			client:  "zcode",
			kind:    core.FailureWAF,
			message: "[zcode] anthropic envelope code 3012",
			want:    "risk control tripped",
		},
		{
			name:    "zcode model not allowed",
			client:  "zcode",
			message: "[zcode] envelope code 3006",
			want:    "not allowed for this account",
		},
		{
			name:    "qwenwork out of credits",
			client:  "qwenwork",
			kind:    core.FailureQuota,
			message: "[qwenwork] upstream 14018",
			want:    "out of credits",
		},
		{
			name:    "kimi cli missing",
			client:  "kimi",
			kind:    core.FailureAuth,
			message: "[kimi] the kimi CLI was not found on PATH",
			want:    "CLI is missing",
		},
		{
			name:    "kimi subscription does not cover the product",
			client:  "kimi",
			message: "[kimi] HTTP 403 access_terminated_error",
			want:    "plan is not",
		},
		{
			name:    "kimi concurrency saturated",
			client:  "kimi",
			message: "[kimi] waiting for a free slot (max_concurrency=2)",
			want:    "concurrency slot",
		},
		{
			name:    "tabbit sidecar model unavailable",
			client:  "tabbit",
			message: `[tabbit] sidecar said {"error":"model_unavailable"}`,
			want:    "model is unavailable",
		},
		{
			name:    "tabbit bracketed sidecar code",
			client:  "tabbit",
			message: "[tabbit] sidecar replied [492]",
			want:    "model_unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := For(tc.client, tc.kind, tc.message)
			if got == "" {
				t.Fatalf("For(%q, %q, %q) = \"\", want a hint containing %q",
					tc.client, tc.kind, tc.message, tc.want)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("For(%q, %q, %q) =\n  %s\nwant it to contain %q",
					tc.client, tc.kind, tc.message, got, tc.want)
			}
		})
	}
}

// A vendor code must match as a whole token.  "11005" is not "1005"; matching on
// a bare substring would attach trae's twelve-hour plan-limit advice to an
// unrelated failure, which is worse than saying nothing.
func TestCodesOnlyMatchWholeDigitRuns(t *testing.T) {
	got := For("trae", "", "upstream code 11005")
	if strings.Contains(got, "failover is deliberately disabled") {
		t.Fatalf("code 1005 matched inside 11005: %q", got)
	}
}

// The same guard applies on the left edge: a longer number must not leak either.
func TestCodesDoNotMatchAsTheTailOfALongerNumber(t *testing.T) {
	for _, msg := range []string{"upstream code 31005", "upstream code 10053"} {
		got := For("trae", "", msg)
		if strings.Contains(got, "failover is deliberately disabled") {
			t.Errorf("%q: code 1005 matched inside a longer number: %q", msg, got)
		}
	}
}

// An unclassified failure still has a client name and a message, and those are
// often enough to be useful — zcode's 3012 arrives from several paths.
func TestAdviceSurvivesAMissingKind(t *testing.T) {
	got := For("zcode", "", "[zcode] risk control 3012")
	if !strings.Contains(got, "risk control tripped") {
		t.Errorf("kind was not needed to classify this failure, got %q", got)
	}
}

// A client rule must be able to override the generic wording for its kind,
// otherwise the specific advice in the table is unreachable.
func TestClientRulesBeatTheGenericKind(t *testing.T) {
	trae := For("trae", core.FailureQuota, "[trae] llm_utils_chat returned 1001")
	generic := For("nosuchclient", core.FailureQuota, "[nosuchclient] llm_utils_chat returned 1001")
	if trae == "" || generic == "" {
		t.Fatalf("both should resolve: trae=%q generic=%q", trae, generic)
	}
	if trae == generic {
		t.Errorf("trae's own quota rule must differ from the generic one; both were %q", trae)
	}
	if !strings.Contains(trae, "credits are exhausted") {
		t.Errorf("the client rule did not win: %q", trae)
	}
}

// When a client has no rule for a kind, it must fall back to the generic one
// rather than going silent.
func TestClientFallsBackToGenericWhenItHasNoRule(t *testing.T) {
	got := For("trae", core.FailureWAF, "[trae] block page")
	if got == "" {
		t.Fatal("trae has no WAF rule of its own, so the generic one must apply")
	}
	if got != For("nosuchclient", core.FailureWAF, "[nosuchclient] block page") {
		t.Errorf("fallback produced different advice: %q", got)
	}
}

// Every classified kind that can reach the API gets a sentence, so an operator
// never sees a typed error with an empty hint.
func TestEveryClassifiedKindHasGenericAdvice(t *testing.T) {
	kinds := []core.FailureKind{
		core.FailureWAF,
		core.FailureRateLimited,
		core.FailureQuota,
		core.FailureAuth,
		core.FailureSessionDead,
		core.FailureUpstream,
		core.FailureContentBlocked,
	}
	for _, k := range kinds {
		if got := For("nosuchclient", k, "something failed"); got == "" {
			t.Errorf("kind %q has no generic advice", k)
		}
	}
}

// FailureOther means "cannot even attempt this", and its message is already the
// instruction.  Inventing advice here would be noise.
func TestOtherIsLeftAlone(t *testing.T) {
	if got := For("nosuchclient", core.FailureOther, "[x] messages[0].role is required"); got != "" {
		t.Errorf("FailureOther should carry no hint, got %q", got)
	}
}

// An unknown failure must stay silent rather than emit a platitude: a hint that
// always appears is a hint operators learn to skip.
func TestUnknownFailureSaysNothing(t *testing.T) {
	if got := For("nosuchclient", "", "connection reset by peer"); got != "" {
		t.Errorf("want no hint for an unknown failure, got %q", got)
	}
}

// Modules embed vendor text in their own casing; the table must not care.
func TestMatchingIsCaseInsensitive(t *testing.T) {
	lower := For("kimi", "", "the kimi cli was not found on path")
	upper := For("kimi", "", "THE KIMI CLI WAS NOT FOUND ON PATH")
	if lower == "" || lower != upper {
		t.Errorf("casing changed the answer: %q vs %q", lower, upper)
	}
}

// A rule that lists two substrings requires both, so a single common word
// cannot drag unrelated failures into its advice.
func TestMultipleSubstringsAllHaveToMatch(t *testing.T) {
	if got := For("tabbit", "", "[tabbit] the welcome banner was shown"); got != "" {
		t.Errorf("a half-matching rule fired: %q", got)
	}
	both := For("tabbit", "", "[tabbit] welcome banner on the free tier")
	if !strings.Contains(both, "free-tier") {
		t.Errorf("both substrings were present but the rule did not fire: %q", both)
	}
}
