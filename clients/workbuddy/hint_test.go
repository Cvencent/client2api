package workbuddy

import (
	"strings"
	"testing"

	"client2api/internal/core"
)

func TestWorkbuddyGatewayHintKindTable(t *testing.T) {
	tests := []struct {
		kind ErrKind
		want string
	}{
		{ErrPromptTooLong, "request context exceeds the model's limit; reduce history/message size"},
		{ErrImageInvalid, "image request was rejected by upstream; check image_url format and image data"},
		{ErrWafBlock, "upstream WAF blocked the gateway; retry after the block window"},
		{ErrSoftRate, "rate limited by upstream; retry after reset"},
		{ErrAccountFault, "platform risk control blocked this account; wait for the restriction to expire or appeal it with the vendor; re-login will not clear it"},
		{ErrSessionDead, "account session expired at upstream; the account is disabled until re-login"},
		{ErrHardCredit, "account credits exhausted at upstream; waiting for daily check-in to restore"},
		{ErrModelBlocked, "upstream has no such model on this backend; switch model or retry on another account"},
		{ErrContentBlocked, "request content was rejected by content policy; adjust the prompt and retry"},
	}
	for _, tc := range tests {
		t.Run(tc.kind.String(), func(t *testing.T) {
			if got := GatewayHint(tc.kind, "whatever the vendor said", HintContext{}); got != tc.want {
				t.Fatalf("GatewayHint(%s) = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}

// An uncovered kind has to yield "" so the caller omits the field, rather than
// shipping a vague "something went wrong".
func TestWorkbuddyGatewayHintIsEmptyForUncoveredKinds(t *testing.T) {
	for _, kind := range []ErrKind{ErrNone, ErrNotFound, ErrServer, ErrBadParams, ErrClient} {
		if got := GatewayHint(kind, "plain failure", HintContext{}); got != "" {
			t.Fatalf("GatewayHint(%s) = %q, want \"\"", kind, got)
		}
	}
}

// The 11133 family wins over the classified kind, because its HTTP status
// classifies as something generic and only the body says what really happened.
func TestWorkbuddyGatewayHintModelParamFamily(t *testing.T) {
	spellings := []string{
		`{"code":11133}`,
		`{"code": 11133}`,
		`{"code":"11133"}`,
		`{"code": "11133"}`,
		`{"code":" 11133"}`,
		`{"code": '11133'}`,
		`{"msg":"model_param_invalid"}`,
		`{"msg":"Invalid request parameters"}`,
		`{"msg":"request parameters do not meet the current model requirements"}`,
	}
	const generic = "request parameters were rejected by the model provider; check message format and model capabilities"
	for _, msg := range spellings {
		// ErrContentBlocked would normally produce its own text: the family must
		// still win.
		if got := GatewayHint(ErrContentBlocked, msg, HintContext{}); got != generic {
			t.Fatalf("GatewayHint(%q) = %q, want the 11133 advice", msg, got)
		}
	}
}

// With an image on a catalogued model that does not support images, the advice
// names the model and where to find a better one.
func TestWorkbuddyGatewayHintNamesAnImageBlindModel(t *testing.T) {
	ctx := HintContext{Model: "deepseek-v3", HasImage: true, ModelInCatalog: true, ModelSupportsImages: false}
	want := "model deepseek-v3 does not support images; pick one with supports_images=true from /v1/models"
	if got := GatewayHint(ErrBadParams, `{"code":11133}`, ctx); got != want {
		t.Fatalf("GatewayHint = %q, want %q", got, want)
	}

	// Any of the three conditions missing falls back to the generic advice.
	generic := "request parameters were rejected by the model provider; check message format and model capabilities"
	for name, ctx := range map[string]HintContext{
		"no image":         {Model: "deepseek-v3", ModelInCatalog: true},
		"not in catalogue": {Model: "deepseek-v3", HasImage: true},
		"supports images":  {Model: "gpt-5", HasImage: true, ModelInCatalog: true, ModelSupportsImages: true},
		"zero value":       {},
	} {
		if got := GatewayHint(ErrBadParams, `{"code":11133}`, ctx); got != generic {
			t.Fatalf("%s: GatewayHint = %q, want the generic 11133 advice", name, got)
		}
	}
}

func TestWorkbuddyGatewayHintInvalidImageFamily(t *testing.T) {
	const want = "image data rejected by upstream; use a real/valid image, may need a new conversation"
	for _, msg := range []string{
		`{"code":11135}`,
		`{"msg":"invalid_image_data"}`,
		`{"msg":"please replace the image and retry"}`,
	} {
		if got := GatewayHint(ErrSoftRate, msg, HintContext{}); got != want {
			t.Fatalf("GatewayHint(%q) = %q, want the 11135 advice", msg, got)
		}
	}
}

// The content-policy wording deliberately avoids the word "upstream": it is the
// one hint an end user sees verbatim.
func TestWorkbuddyGatewayHintContentBlockedHidesInternals(t *testing.T) {
	got := GatewayHint(ErrContentBlocked, "blocked", HintContext{})
	if strings.Contains(strings.ToLower(got), "upstream") {
		t.Fatalf("the content-policy hint leaks the gateway: %q", got)
	}
}

func TestWorkbuddyCodeMarkerSpellings(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{`"code":11133`, true},
		{`"code": 11133`, true},
		{`"code":"11133"`, true},
		{`"code": "11133"`, true},
		{`"code":" 11133"`, true},
		{`"code": '11133'`, true},
		{`"code":111331`, true}, // the marker is a substring, so a longer code matches too
		{`"code":1113`, false},
		{`"msg":"11133"`, false},
		{`code=11133`, false},
		{``, false},
	}
	for _, tc := range tests {
		if got := codeMarker(strings.ToLower(tc.body), "11133"); got != tc.want {
			t.Errorf("codeMarker(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
	// The match is case-insensitive in practice because callers lower-case first.
	if !codeMarker(strings.ToLower(`{"CODE": 11135}`), "11135") {
		t.Error("codeMarker missed an upper-case CODE key")
	}
}

func TestWorkbuddyFrameKind(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    ErrKind
	}{
		{"a rate-limit frame", `{"code":6004,"msg":"rate limited"}`, ErrSoftRate},
		{"a model-not-found frame", `{"error":{"message":"the model does not exist"}}`, Classify(400, "the model does not exist")},
		{"a context-length frame", `{"error":{"message":"context length exceeded"}}`, Classify(400, "context length exceeded")},
		{"an empty error message", `{"error":{"message":"   "}}`, ErrNone},
		{"no error block", `{"choices":[]}`, ErrNone},
		{"not json at all", `oops`, ErrNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FrameKind(tc.payload); got != tc.want {
				t.Fatalf("FrameKind(%q) = %s, want %s", tc.payload, got, tc.want)
			}
		})
	}
}

func TestWorkbuddyFrameHintFuncIsLazyAndSkipsSentinels(t *testing.T) {
	calls := 0
	hint := FrameHintFunc(func() HintContext {
		calls++
		return HintContext{}
	})
	if calls != 0 {
		t.Fatalf("ctxFn was called %d times at construction, want 0", calls)
	}
	if got := hint(""); got != "" {
		t.Fatalf("hint(\"\") = %q, want \"\"", got)
	}
	if got := hint("[DONE]"); got != "" {
		t.Fatalf("hint([DONE]) = %q, want \"\"", got)
	}
	if calls != 0 {
		t.Fatalf("ctxFn was called %d times for non-error payloads, want 0", calls)
	}

	var seenCtx HintContext
	hint = FrameHintFunc(func() HintContext {
		seenCtx = HintContext{Model: "deepseek-v3", HasImage: true, ModelInCatalog: true}
		return seenCtx
	})
	got := hint(`{"code":11133}`)
	if got == "" {
		t.Fatal("an error frame produced no hint")
	}
	if seenCtx.Model != "deepseek-v3" {
		t.Fatalf("ctxFn did not run: %+v", seenCtx)
	}
}

func TestWorkbuddyNoHealthyAccountHint(t *testing.T) {
	const want = "no healthy account available in pool; check /status or retry later"
	if got := NoHealthyAccountHint(); got != want {
		t.Fatalf("NoHealthyAccountHint() = %q, want %q", got, want)
	}
}

// Hint is the module's core.HintProvider face.  It has to translate the shared
// failure vocabulary into this vendor's kind table before answering, or every
// classified failure would land in the default branch and go silent.
func TestWorkbuddyHintTranslatesTheSharedKinds(t *testing.T) {
	c := &Client{}
	tests := []struct {
		kind core.FailureKind
		want string
	}{
		{core.FailureWAF, "upstream WAF blocked the gateway; retry after the block window"},
		{core.FailureRateLimited, "rate limited by upstream; retry after reset"},
		{core.FailureQuota, "account credits exhausted at upstream; waiting for daily check-in to restore"},
		{core.FailureAuth, "platform risk control blocked this account; wait for the restriction to expire or appeal it with the vendor; re-login will not clear it"},
		{core.FailureSessionDead, "account session expired at upstream; the account is disabled until re-login"},
		{core.FailureContentBlocked, "request content was rejected by content policy; adjust the prompt and retry"},
	}
	for _, tc := range tests {
		if got := c.Hint(tc.kind, "whatever the vendor said", core.HintContext{}); got != tc.want {
			t.Errorf("Hint(%s) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// A kind this vendor has no wording for must come back empty, so the gateway
// hands the question to the shared table instead of shipping a vague sentence.
func TestWorkbuddyHintStaysSilentForUnmappedKinds(t *testing.T) {
	c := &Client{}
	for _, kind := range []core.FailureKind{core.FailureUpstream, core.FailureOther} {
		if got := c.Hint(kind, "plain failure", core.HintContext{}); got != "" {
			t.Errorf("Hint(%s) = %q, want \"\"", kind, got)
		}
	}
}

// The context the gateway resolved must survive the translation: the 11133
// family can only name a model when HasImage, ModelInCatalog and
// ModelSupportsImages all come through.
func TestWorkbuddyHintCarriesTheGatewayContext(t *testing.T) {
	c := &Client{}
	ctx := core.HintContext{
		Client:              "workbuddy",
		Model:               "deepseek-v3",
		HasImage:            true,
		ModelInCatalog:      true,
		ModelSupportsImages: false,
	}
	want := "model deepseek-v3 does not support images; pick one with supports_images=true from /v1/models"
	if got := c.Hint(core.FailureRateLimited, `{"code":11133}`, ctx); got != want {
		t.Fatalf("Hint = %q, want %q", got, want)
	}
}

// The vendor's own body codes still win over the translated kind, exactly as
// they do on the direct GatewayHint path.
func TestWorkbuddyHintKeepsTheBodyCodeFamilies(t *testing.T) {
	c := &Client{}
	// 11135 is not one of the translated kinds, so this also proves the family
	// check runs before the kind switch.
	want := "image data rejected by upstream; use a real/valid image, may need a new conversation"
	if got := c.Hint(core.FailureOther, `{"code":11135}`, core.HintContext{}); got != want {
		t.Fatalf("Hint = %q, want %q", got, want)
	}
}
