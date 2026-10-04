package core

import (
	"context"
	"testing"
)

// captchaClient opted into the browser-token contract and nothing else.
type captchaClient struct{ plainClient }

func (c *captchaClient) CaptchaScene(context.Context, string) (CaptchaScene, error) {
	return CaptchaScene{Required: true, SceneID: "s1"}, nil
}

// TestWithCaptchaSolutionRoundTrips is the contract every module relies on: what
// the panel attaches is what the module reads back.
func TestWithCaptchaSolutionRoundTrips(t *testing.T) {
	ctx := WithCaptchaSolution(context.Background(), CaptchaSolution{Param: "tok", Region: "cn"})

	got, ok := CaptchaSolutionFrom(ctx)
	if !ok {
		t.Fatal("CaptchaSolutionFrom reported nothing after WithCaptchaSolution")
	}
	if got.Param != "tok" || got.Region != "cn" {
		t.Fatalf("got %+v, want {Param:tok Region:cn}", got)
	}
}

// TestWithCaptchaSolutionTrimsTheValue pins the trim.  The token arrives either
// from a browser form field or from an executable's stdout, and in the second
// case a trailing newline is the normal shape of the value, not an exotic one.
// An untrimmed token would be sent verbatim in a header and rejected by the
// vendor, which would look like a wrong token rather than a stray byte.
func TestWithCaptchaSolutionTrimsTheValue(t *testing.T) {
	ctx := WithCaptchaSolution(context.Background(), CaptchaSolution{
		Param:  "  tok\n",
		Region: " cn ",
	})

	got, ok := CaptchaSolutionFrom(ctx)
	if !ok {
		t.Fatal("a padded token was treated as absent")
	}
	if got.Param != "tok" {
		t.Errorf("Param = %q, want %q", got.Param, "tok")
	}
	if got.Region != "cn" {
		t.Errorf("Region = %q, want %q", got.Region, "cn")
	}
}

// TestWithCaptchaSolutionIgnoresABlankParam is the load-bearing half of the
// contract.  A panel that always sends the fields must not make "the operator
// solved nothing" distinguishable from "there was nothing to solve": the module
// has to fall through to its own means in both cases, and a solution that
// existed but was empty would silently suppress the solver.
func TestWithCaptchaSolutionIgnoresABlankParam(t *testing.T) {
	cases := []struct {
		name string
		sol  CaptchaSolution
	}{
		{"an empty struct", CaptchaSolution{}},
		{"an empty param", CaptchaSolution{Param: ""}},
		{"a whitespace param", CaptchaSolution{Param: "  \t\n"}},
		{"a region with no param", CaptchaSolution{Region: "cn"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := context.Background()
			ctx := WithCaptchaSolution(base, tc.sol)

			// Identity, not just absence: a module that checks "did a value
			// arrive" must not see a wrapper it cannot unwrap.
			if ctx != base {
				t.Error("the context was wrapped even though there was no token")
			}
			if _, ok := CaptchaSolutionFrom(ctx); ok {
				t.Error("CaptchaSolutionFrom reported a token that was never minted")
			}
		})
	}
}

// TestCaptchaSolutionFromOnABareContext covers the ordinary case: a call that
// never went near a browser.
func TestCaptchaSolutionFromOnABareContext(t *testing.T) {
	if got, ok := CaptchaSolutionFrom(context.Background()); ok {
		t.Fatalf("got %+v, want nothing", got)
	}
}

// TestAsCaptchaProviderFollowsTheCapabilityConvention checks the narrowing
// helper both ways.  The panel lights the browser step from exactly this
// assertion, so a module that implements the interface but is not recognised
// would look like a missing feature rather than a broken one.
func TestAsCaptchaProviderFollowsTheCapabilityConvention(t *testing.T) {
	var c Client = &captchaClient{plainClient{name: "cap"}}
	if _, ok := AsCaptchaProvider(c); !ok {
		t.Error("a client implementing CaptchaProvider was not recognised")
	}

	var plain Client = &plainClient{name: "plain"}
	if _, ok := AsCaptchaProvider(plain); ok {
		t.Error("a client without CaptchaProvider was recognised")
	}
}

// TestCapabilitiesOfReportsTheCaptchaOptIn keeps the flag and the interface in
// step.  The panel reads the flag, not the interface, so the two drifting apart
// would disable the browser step for a module that supports it.
func TestCapabilitiesOfReportsTheCaptchaOptIn(t *testing.T) {
	ctx := context.Background()
	if caps := CapabilitiesOf(ctx, &captchaClient{plainClient{name: "cap"}}); !caps.Captcha {
		t.Error("Captcha = false for a client implementing CaptchaProvider")
	}
	if caps := CapabilitiesOf(ctx, &plainClient{name: "plain"}); caps.Captcha {
		t.Error("Captcha = true for a client implementing nothing")
	}
}
