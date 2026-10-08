package workbuddy

import (
	"encoding/json"
	"net/http"
	"strings"

	"client2api/internal/core"
)

// Gateway hints: actionable English advice attached to an upstream error so a
// client that only sees the raw vendor message can still act on it.
//
// Ported from the reference internal/upstream/hint.go (MIT).  Two rules from the
// reference are preserved verbatim and must not be "improved" later:
//
//  1. A hint NEVER replaces error.message.  It is a sibling field; dropping or
//     rewriting the vendor's own text loses the only evidence an operator has.
//  2. An uncovered error kind yields "" and the caller then omits the field
//     entirely, rather than shipping a generic "something went wrong".
//
// Wiring: this module owns the vendor vocabulary, so it implements
// core.HintProvider and the gateway asks it first, falling back to the shared
// internal/hint table when the module has nothing to say.  The gateway is the
// only layer that sees both the classified kind and the request that failed, so
// it assembles the core.HintContext (bare model name, whether an image part was
// present, and what the catalogue states about that model) and hands it over.

// HintContext is everything the hint generator needs to know about the request
// that failed.  The zero value is legal and simply yields the generic advice.
type HintContext struct {
	Model               string
	HasImage            bool
	ModelSupportsImages bool
	ModelInCatalog      bool
}

// noHealthyHint is handed out when the pool has nothing to try at all.
const noHealthyHint = "no healthy account available in pool; check /status or retry later"

// NoHealthyAccountHint is the advice for "the pool is empty".
func NoHealthyAccountHint() string { return noHealthyHint }

// GatewayHint turns an upstream error kind plus its message into actionable
// advice.  Order matters: the vendor's own business codes (11133/11135) are
// checked before the classified kind, because those two arrive with a status
// that classifies as something generic and only the body says what went wrong.
// Returns "" when nothing useful can be said.
func GatewayHint(kind ErrKind, msg string, ctx HintContext) string {
	lower := strings.ToLower(msg)

	// Family 11133: the model provider rejected the parameters.
	if isModelParamInvalid(lower) {
		if ctx.HasImage && ctx.ModelInCatalog && !ctx.ModelSupportsImages {
			return "model " + ctx.Model + " does not support images; pick one with supports_images=true from /v1/models"
		}
		return "request parameters were rejected by the model provider; check message format and model capabilities"
	}
	// Family 11135: the image payload itself was rejected.
	if isInvalidImageData(lower) {
		return "image data rejected by upstream; use a real/valid image, may need a new conversation"
	}

	switch kind {
	case ErrPromptTooLong:
		return "request context exceeds the model's limit; reduce history/message size"
	case ErrImageInvalid:
		return "image request was rejected by upstream; check image_url format and image data"
	case ErrWafBlock:
		return "upstream WAF blocked the gateway; retry after the block window"
	case ErrSoftRate:
		return "rate limited by upstream; retry after reset"
	case ErrAccountFault:
		return "platform risk control blocked this account; wait for the restriction to expire or appeal it with the vendor; re-login will not clear it"
	case ErrSessionDead:
		return "account session expired at upstream; the account is disabled until re-login"
	case ErrHardCredit:
		return "account credits exhausted at upstream; waiting for daily check-in to restore"
	case ErrModelBlocked:
		return "upstream has no such model on this backend; switch model or retry on another account"
	case ErrContentBlocked:
		return "request content was rejected by content policy; adjust the prompt and retry"
	default:
		return ""
	}
}

// Hint implements core.HintProvider so the gateway can attach this module's
// advice to a live upstream failure.  It is a thin adapter: the taxonomy the
// gateway classifies with (core.FailureKind) is coarser than this module's own
// ErrKind, and the 11133/11135 families are decided from the message body
// before the kind is consulted, so the lossy mapping below costs nothing that
// matters -- the two vendor-code families still fire on their own evidence.
//
// A kind the mapping cannot express becomes ErrNone, which falls through to
// "" and lets the gateway use the shared table instead.
func (c *Client) Hint(kind core.FailureKind, message string, ctx core.HintContext) string {
	return GatewayHint(errKindFor(kind), message, HintContext{
		Model:               ctx.Model,
		HasImage:            ctx.HasImage,
		ModelSupportsImages: ctx.ModelSupportsImages,
		ModelInCatalog:      ctx.ModelInCatalog,
	})
}

// errKindFor maps the gateway's coarse failure taxonomy onto this module's
// finer upstream vocabulary.  It is deliberately one-way and lossy: several
// ErrKind values collapse onto one FailureKind, and the ones with no gateway
// counterpart (ErrPromptTooLong, ErrImageInvalid, ErrModelBlocked) are reached
// through the message body rather than through the classified kind.
func errKindFor(kind core.FailureKind) ErrKind {
	switch kind {
	case core.FailureWAF:
		return ErrWafBlock
	case core.FailureRateLimited:
		return ErrSoftRate
	case core.FailureQuota:
		return ErrHardCredit
	case core.FailureAuth:
		return ErrAccountFault
	case core.FailureSessionDead:
		return ErrSessionDead
	case core.FailureContentBlocked:
		return ErrContentBlocked
	default:
		return ErrNone
	}
}

// isModelParamInvalid matches the 11133 family.
func isModelParamInvalid(lower string) bool {
	return codeMarker(lower, "11133") ||
		strings.Contains(lower, "model_param_invalid") ||
		strings.Contains(lower, "invalid request parameters") ||
		strings.Contains(lower, "request parameters do not meet the current model requirements")
}

// isInvalidImageData matches the 11135 family.
func isInvalidImageData(lower string) bool {
	return codeMarker(lower, "11135") ||
		strings.Contains(lower, "invalid_image_data") ||
		strings.Contains(lower, "replace the image")
}

// codeMarker matches the several spellings a business code takes in a vendor
// message: bare, spaced, quoted, space-quoted and single-quoted.
func codeMarker(lower, code string) bool {
	for _, form := range []string{
		`"code":` + code,
		`"code": ` + code,
		`"code":"` + code + `"`,
		`"code": "` + code + `"`,
		`"code":" ` + code + `"`,
		`"code": '` + code + `'`,
	} {
		if strings.Contains(lower, form) {
			return true
		}
	}
	return false
}

// FrameKind classifies one SSE error frame payload.  A rate-limit frame is
// recognised by its own body shape before parsing; anything without a parseable
// error.message is ErrNone, i.e. "not an error frame".
func FrameKind(payload string) ErrKind {
	if IsModelRateLimit(payload) {
		return ErrSoftRate
	}
	var frame struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		return ErrNone
	}
	if strings.TrimSpace(frame.Error.Message) == "" {
		return ErrNone
	}
	return Classify(http.StatusBadRequest, frame.Error.Message)
}

// FrameHintFunc adapts the hint generator to a streaming error frame.  ctxFn is
// lazy on purpose: it reaches into the request's model catalogue, and calling it
// for every delta would be pure waste.  Empty payloads and the [DONE] sentinel
// are not errors and produce no hint.
func FrameHintFunc(ctxFn func() HintContext) func(string) string {
	return func(payload string) string {
		if payload == "" || payload == "[DONE]" {
			return ""
		}
		return GatewayHint(FrameKind(payload), payload, ctxFn())
	}
}
