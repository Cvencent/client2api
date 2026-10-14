// Package gemini implements the Gemini (Google Cloud Code Assist) upstream --
// the free line the Antigravity IDE rides on.
//
// Four facts from the reference implementation shape every file here, and
// three of them are byte-level constraints rather than preferences:
//
//  1. The identity is FIVE header constants, written verbatim.  A single
//     character different may be read as "not the official client", and the
//     reference deliberately sends NO x-goog-api-key / x-goog-api-client.
//  2. The request envelope is serialised layer-by-layer in ALPHABETICAL key
//     order.  Go's encoding/json already sorts a map's keys, which is why the
//     body is built from map[string]any rather than from structs.
//  3. The model name is the admission key: a bare `gemini-3.8-flash` answers
//     404, and only `gemini-3.8-flash-<tier>` is served.  The tier suffix is
//     the reasoning effort, not a separate model.
//  4. `includeThoughts` is always true.  "Turning thinking off" is a lie the
//     vendor bills through anyway, so the module never offers a `none` tier.
//
// Credentials are Google OAuth tokens minted through a browser hand-off, and
// the quota surface is a pair of rolling windows (5-hour + weekly), not a
// credit balance -- see balance.go.
package gemini

// ---------------------------------------------------------------------------
// product constants
// ---------------------------------------------------------------------------

const (
	// clientName is the routing prefix and the name the panel shows.
	clientName = "gemini"

	// credentialsFile is the credential store, relative to Deps.DataDir.
	credentialsFile = "accounts.json"
	// sigFile caches thoughtSignature values, relative to Deps.DataDir.
	sigFile = "signatures.json"

	// streamPath is the SSE inference path.
	streamPath = "/v1internal:streamGenerateContent?alt=sse"
	// generatePath is the non-streaming inference path.
	generatePath = "/v1internal:generateContent"
	// quotaPath is the quota-window summary path.
	quotaPath = "/v1internal:retrieveUserQuotaSummary"
	// loadCodeAssistPath reports the account tier and project.
	loadCodeAssistPath = "/v1internal:loadCodeAssist"

	// loadCodeAssistBody is the tier-probe body, copied verbatim (its length is
	// exactly 38 bytes upstream).  Adding a field risks being read as official
	// client detection, so it stays a constant.
	loadCodeAssistBody = `{"metadata":{"ideType":"ANTIGRAVITY"}}`

	// defaultProject is the fallback project id.  The real value comes from
	// loadCodeAssist; this is only used when the probe answers empty.
	defaultProject = "aicode-consumers"

	// upstreamUserAgent is the envelope's userAgent field (same value as the
	// x-client-name header).
	upstreamUserAgent = "antigravity"
)

// endpoints are the two inference hosts.  They are variables, not constants,
// so a test can point them at a fake server; production never reassigns them.
var (
	// endpointDaily is the primary inference endpoint.
	endpointDaily = "https://daily-cloudcode-pa.googleapis.com"
	// endpointSandbox is the fallback endpoint.  The reference rotates between
	// the two; the difference is not proven, but alternating is free.
	endpointSandbox = "https://daily-cloudcode-pa.sandbox.googleapis.com"
)

// identityHeaders are the five upstream identity headers, written verbatim.
// They are lower-cased keys because Go canonicalises them on the wire anyway.
var identityHeaders = map[string]string{
	"User-Agent":         "antigravity/4.3.0 (cmdc-pak)",
	"x-client-name":      "antigravity",
	"x-client-version":   "4.3.0",
	"x-machine-id":       "cmdc-pak",
	"x-vscode-sessionid": "proxy",
}

// ---------------------------------------------------------------------------
// model table
// ---------------------------------------------------------------------------

const (
	// upstreamFlash is the bare upstream model name.  The served name carries a
	// tier suffix.
	upstreamFlash = "gemini-3.8-flash"

	// ContextWindow is the context window the panel advertises.
	contextWindow = 1_000_000
	// maxOutputTokens is the default and ceiling output budget.
	maxOutputTokens = 64_000

	// Thinking budgets per tier.  The name is a label; the budget is the
	// behaviour.
	budgetLow    = 1_000
	budgetMedium = 4_000
	budgetHigh   = 10_000
	// budgetTiered (-1) means "omit thinkingBudget": the vendor adapts.
	budgetTiered = -1
)

// effortIDs is the reasoning vocabulary this model accepts.  There is
// deliberately no "none": a "disabled" thinking request still bills tokens.
var effortIDs = []string{"low", "medium", "high", "tiered"}

// defaultEffort is the tier applied when the caller names none.
const defaultEffort = "medium"

// effortLabel is the Chinese display name the reference uses.
func effortLabel(id string) string {
	switch id {
	case "low":
		return "低"
	case "medium":
		return "中"
	case "high":
		return "高"
	case "tiered":
		return "自适应"
	default:
		return id
	}
}

// canonicalModelID strips a known tier suffix, so a rate-limit marker or a
// probe that recorded the suffixed name still matches the catalogue.
func canonicalModelID(modelID string) string {
	for _, tier := range effortIDs {
		if suffix := "-" + tier; len(modelID) > len(suffix) && modelID[len(modelID)-len(suffix):] == suffix {
			return modelID[:len(modelID)-len(suffix)]
		}
	}
	return modelID
}

// effortToTier normalises an effort value; anything unknown becomes medium,
// matching the reference's default branch.
func effortToTier(effort string) string {
	switch effort {
	case "low", "medium", "high", "tiered":
		return effort
	default:
		return defaultEffort
	}
}

// thinkingBudget maps a tier onto its budget, or -1 for the adaptive tier.
func thinkingBudget(tier string) int {
	switch tier {
	case "low":
		return budgetLow
	case "high":
		return budgetHigh
	case "tiered":
		return budgetTiered
	default:
		return budgetMedium
	}
}

// upstreamModelName is the admission key: the bare model plus the tier suffix.
func upstreamModelName(tier string) string { return upstreamFlash + "-" + tier }
