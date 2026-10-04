// Package hint turns a failed chat request into one actionable English
// sentence for the operator.
//
// The modules already know *what* went wrong, and the gateway already knows
// how to map that onto a status code and an error type.  What no layer owned
// was the third thing an operator needs: what to do next.  A raw vendor code
// ("3012", "1005", "service info not found") is unreadable on its own, and the
// cheapest place to attach the advice is the error envelope itself, where the
// panel and the API caller both already look.
//
// The table is deliberately conservative.  A rule only exists where the advice
// is genuinely determined by the failure, and an unmatched failure yields no
// hint rather than a generic platitude: "please retry" on a quota error trains
// the operator to ignore the field.
package hint

import (
	"strings"

	"client2api/internal/core"
)

// rule matches a failure and supplies the advice.  All of codes and contains
// must match when non-empty; an empty slice is a wildcard for that dimension.
type rule struct {
	// client scopes the rule to one module.  Client-specific rules are
	// evaluated first so a client can override a generic wording.
	client string
	// kind is the classified failure kind.  Empty means any kind.
	kind core.FailureKind
	// codes are vendor status codes, matched as standalone digit tokens.
	codes []string
	// contains are lowercased substrings that must all appear in the message.
	contains []string
	hint     string
}

// generic rules apply to every client, keyed only by the classified kind.
var generic = []rule{
	{
		kind: core.FailureWAF,
		hint: "The vendor is blocking this egress IP, so switching accounts will not " +
			"help. Check the guard block on /v1/status, reduce the request rate, or " +
			"route through a different egress.",
	},
	{
		kind: core.FailureRateLimited,
		hint: "The account was rate-limited. The gateway already paces retries; " +
			"lower concurrency or add another account to spread the load.",
	},
	{
		kind: core.FailureQuota,
		hint: "The account is out of quota. Retrying it will not help: add an " +
			"account or wait for the quota to reset.",
	},
	{
		kind: core.FailureAuth,
		hint: "The credential was rejected. Sign in again for this account in the " +
			"panel.",
	},
	{
		kind: core.FailureSessionDead,
		hint: "The vendor session is gone. Sign in again for this account in the " +
			"panel.",
	},
	{
		kind: core.FailureUpstream,
		hint: "The vendor returned an upstream error. Retry; if it persists the " +
			"vendor is degraded rather than your configuration.",
	},
	{
		kind: core.FailureContentBlocked,
		hint: "The vendor's content filter rejected the request. It was already " +
			"retried once with a neutral system prompt; rephrase the request.",
	},
}

// specific rules name the client and usually the vendor code.  They are checked
// before the generic ones.
var specific = []rule{
	// ---- workbuddy -------------------------------------------------------
	{
		client:   "workbuddy",
		codes:    []string{"14051"},
		contains: []string{"trial"},
		hint: "This account has already claimed its trial. Use a different " +
			"account for the trial reward.",
	},
	{
		client:   "workbuddy",
		contains: []string{"trial", "not activated"},
		hint: "The trial needs to be activated before this country path is " +
			"available. Run the global registration flow for this account in the " +
			"panel.",
	},
	{
		client: "workbuddy",
		codes:  []string{"11102"},
		hint: "This account cannot serve this model. The account/model pair is " +
			"cooling; pick another model or wait out the backoff.",
	},
	{
		client:   "workbuddy",
		contains: []string{"service info not found"},
		hint: "The vendor does not recognise this account/model pair. Try another " +
			"model, or refresh the model list.",
	},

	// ---- trae ------------------------------------------------------------
	{
		client: "trae",
		codes:  []string{"1005"},
		hint: "The plan limit is exhausted for this account. The cooldown is 12 " +
			"hours and failover is deliberately disabled: retrying other accounts " +
			"will not help, and the vendor penalises it.",
	},
	{
		client: "trae",
		codes:  []string{"1001"},
		hint: "The account's credits are exhausted or the account is unavailable. " +
			"Check the credits column in the panel and add an account.",
	},
	{
		client: "trae",
		codes:  []string{"4008"},
		hint: "The vendor rejected the request as unauthenticated or invalid. " +
			"Sign in again for this account.",
	},
	{
		client: "trae",
		codes:  []string{"4010"},
		hint: "The vendor refused this account. Check the account's status in the " +
			"panel before retrying.",
	},
	{
		client: "trae",
		codes:  []string{"4011"},
		hint: "The vendor refused this account. Check the account's status in the " +
			"panel before retrying.",
	},
	{
		client: "trae",
		codes:  []string{"9074"},
		hint: "The vendor refused this request. Retry, and check the error text " +
			"for the specific field it objected to.",
	},

	// ---- zcode -----------------------------------------------------------
	{
		client: "zcode",
		codes:  []string{"3012"},
		hint: "The vendor's risk control tripped. This is usually IP- or " +
			"device-level, so rotating accounts makes it worse: the official CLI " +
			"identity blocks must stay in the body, and the request rate should " +
			"drop before retrying.",
	},
	{
		client: "zcode",
		codes:  []string{"3006"},
		hint: "This model is not allowed for this account. Pick a different model " +
			"rather than retrying the same one.",
	},
	{
		client: "zcode",
		codes:  []string{"3007", "3009"},
		hint: "The request shape was refused by the vendor; the official system " +
			"blocks must be present and unmodified.",
	},

	// ---- qwenwork --------------------------------------------------------
	{
		client: "qwenwork",
		codes:  []string{"14018"},
		hint: "The account is out of credits. Add an account; the exhausted one is " +
			"cooling for a long window.",
	},

	// ---- kimi ------------------------------------------------------------
	{
		client:   "kimi",
		contains: []string{"not found on path"},
		hint: "The kimi CLI is missing. Install it (the module shells out to the " +
			"CLI for every request) or point the module at the binary.",
	},
	{
		client:   "kimi",
		contains: []string{"not configured"},
		hint: "No usable kimi account is bound. Sign in from the panel; the CLI " +
			"binding is what this module authenticates with.",
	},
	{
		client:   "kimi",
		contains: []string{"access_terminated"},
		hint: "The subscription behind this account does not cover the Kimi Code " +
			"endpoint. The credential is valid but the plan is not: this cannot be " +
			"fixed by retrying or by changing headers.",
	},
	{
		client:   "kimi",
		contains: []string{"max_concurrency", "free slot"},
		hint: "Every concurrency slot is busy. Raise max_concurrency or queue the " +
			"request; the CLI cannot run more copies in parallel.",
	},
	{
		client:   "kimi",
		contains: []string{"stream-json"},
		hint: "The CLI returned something this module does not understand. Check " +
			"the installed CLI version against the one the module targets.",
	},

	// ---- tabbit ----------------------------------------------------------
	{
		client:   "tabbit",
		contains: []string{"model_unavailable"},
		hint: "The sidecar says this model is unavailable. Pick another model or " +
			"check the sidecar is healthy.",
	},
	{
		client:   "tabbit",
		contains: []string{"[492]"},
		hint: "The sidecar reported model_unavailable for this request. Try another " +
			"model, or check the sidecar's own log.",
	},
	{
		client:   "tabbit",
		contains: []string{"free tier", "welcome"},
		hint: "The vendor surfaced a free-tier or welcome banner instead of a " +
			"reply. The browser profile in the sidecar may need to be signed in.",
	},
}

// For returns one actionable sentence for a failed request, or "" when the
// failure carries no advice we can stand behind.
//
// kind may be empty when the module did not classify the failure; clientName
// and message are matched against the tables regardless, because a vendor error
// code in the text is often enough to be useful.
func For(clientName string, kind core.FailureKind, message string) string {
	msg := strings.ToLower(message)
	for _, r := range specific {
		if r.client != clientName {
			continue
		}
		if r.matches(kind, msg) {
			return r.hint
		}
	}
	for _, r := range generic {
		if r.matches(kind, msg) {
			return r.hint
		}
	}
	return ""
}

// matches reports whether the rule describes this failure.  Every dimension the
// rule names has to hold: a rule listing both a code and a substring needs
// both, otherwise a bare word like "trial" would pull unrelated failures into
// the wrong advice.
func (r rule) matches(kind core.FailureKind, lower string) bool {
	if r.kind != "" && r.kind != kind {
		return false
	}
	if len(r.codes) > 0 {
		hit := false
		for _, c := range r.codes {
			if hasCode(lower, c) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for _, c := range r.contains {
		if !strings.Contains(lower, c) {
			return false
		}
	}
	// A rule with no criteria at all would match every failure in its scope,
	// which is never what the table intends.
	return r.kind != "" || len(r.codes) > 0 || len(r.contains) > 0
}

// hasCode reports whether the message contains code as a standalone digit run,
// so vendor code 1005 matches "upstream said 1005" but not "11005".
func hasCode(lower, code string) bool {
	for i := 0; ; {
		j := strings.Index(lower[i:], code)
		if j < 0 {
			return false
		}
		at := i + j
		if !digitBefore(lower, at) && !digitAfter(lower, at+len(code)) {
			return true
		}
		i = at + 1
		if i >= len(lower) {
			return false
		}
	}
}

func digitBefore(s string, at int) bool {
	if at == 0 {
		return false
	}
	return s[at-1] >= '0' && s[at-1] <= '9'
}

func digitAfter(s string, at int) bool {
	if at >= len(s) {
		return false
	}
	return s[at] >= '0' && s[at] <= '9'
}
