package core

import "testing"

// hintingClient opts into HintProvider.  It answers for exactly one kind, so a
// test can tell "the module had nothing to say" from "the module was never
// asked".
type hintingClient struct{ plainClient }

func (c *hintingClient) Hint(kind FailureKind, message string, ctx HintContext) string {
	if kind == FailureWAF {
		return "module waf text"
	}
	return ""
}

// TestHintOfFindsOnlyTheModulesThatOptIn pins the whole point of the optional
// interface: a module that never implemented Hint must not look like one, or
// the gateway would treat its silence as a verdict and stop consulting the
// shared rule table.
func TestHintOfFindsOnlyTheModulesThatOptIn(t *testing.T) {
	plain := &plainClient{name: "plain"}
	if _, ok := AsHintProvider(plain); ok {
		t.Fatal("a module without Hint was reported as a HintProvider")
	}
	if got := HintOf(plain); got != nil {
		t.Fatalf("HintOf(plain) = %v, want nil", got)
	}

	hinting := &hintingClient{plainClient{name: "hinting"}}
	p, ok := AsHintProvider(hinting)
	if !ok {
		t.Fatal("a module with Hint was not reported as a HintProvider")
	}
	if got := p.Hint(FailureWAF, "whatever", HintContext{}); got != "module waf text" {
		t.Fatalf("Hint = %q, want the module's text", got)
	}
	if got := HintOf(hinting); got == nil {
		t.Fatal("HintOf(hinting) = nil, want the module")
	}
}

// TestHintProviderEmptyAnswerIsNotAVerdict documents the hand-off contract from
// the provider side: "" is what a module returns when it has no wording for the
// kind, and the caller is expected to keep asking elsewhere.
func TestHintProviderEmptyAnswerIsNotAVerdict(t *testing.T) {
	hinting := &hintingClient{plainClient{name: "hinting"}}
	p, _ := AsHintProvider(hinting)
	if got := p.Hint(FailureQuota, "whatever", HintContext{}); got != "" {
		t.Fatalf("Hint = %q, want \"\"", got)
	}
}

// TestHintOfSurvivesANilClient keeps the accessors panic-free: the gateway calls
// them on whatever the registry handed it, before it knows anything answered.
func TestHintOfSurvivesANilClient(t *testing.T) {
	if _, ok := AsHintProvider(nil); ok {
		t.Fatal("nil was reported as a HintProvider")
	}
	if got := HintOf(nil); got != nil {
		t.Fatalf("HintOf(nil) = %v, want nil", got)
	}
}

// TestHintContextZeroValueClaimsNoCapabilityFact pins the rule the gateway's
// error path depends on: an unresolved context must read as "not known", never
// as "this model cannot see images".
func TestHintContextZeroValueClaimsNoCapabilityFact(t *testing.T) {
	var ctx HintContext
	if ctx.HasImage || ctx.ModelInCatalog || ctx.ModelSupportsImages {
		t.Fatalf("the zero HintContext claims a capability fact: %+v", ctx)
	}
}
