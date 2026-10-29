package gateway

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/livecfg"
)

// A module that returns a bare transport error must not end the whole route.
// The gateway cannot classify a *url.Error (it carries no FailureKind), so
// before this it answered 502 even though the candidate chain held a healthy
// platform.  The live symptom was RACCOON dropping the connection
// (`Post ".../chat/completions": EOF`) while every sibling platform for the
// same model was serving.
func TestChatFailsOverFromAnUnclassifiedTransportError(t *testing.T) {
	// Exactly the shape raccoon handed back: a *url.Error wrapping EOF.
	bareEOF := &url.Error{Op: "Post", URL: "https://xiaohuanxiong.com/api/web/llm/v2/chat/completions", Err: io.ErrUnexpectedEOF}

	alpha := &testClient{
		name:      "alpha",
		catalogue: []core.Model{{ID: "alpha-model"}},
		chatErr:   bareEOF,
	}
	beta := &testClient{
		name:      "beta",
		catalogue: []core.Model{{ID: "beta-model"}},
		servedBy:  "b1",
		events:    usageEvents(3, 4),
	}

	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 1},
		"beta":  {Priority: 2},
	})
	reg.SetModelGroups(map[string]core.ModelGroup{
		"shared": {
			Members: []core.ModelGroupMember{
				{Client: "alpha", Model: "alpha-model"},
				{Client: "beta", Model: "beta-model"},
			},
		},
	})

	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
		Live: livecfg.New(livecfg.Snapshot{
			MaxRotate:         1,
			RotateBackoffBase: time.Nanosecond,
		}),
	})

	rec := chat(t, srv, `{"model":"shared","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s: a bare transport error on the first platform must fail over, not 502", rec.Code, rec.Body.String())
	}
	if beta.seen == nil {
		t.Fatal("the healthy sibling never received the request")
	}
}

// A request-shape refusal is not a transport fault, and normalizing must not
// turn it into a platform-wide retry: the same body would be refused by every
// platform, and the caller still needs the vendor's own 4xx.
func TestTransportNormalizationLeavesAnUnclassifiedRefusalAlone(t *testing.T) {
	refusal := errors.New("alpha: invalid parameter: tools")
	if got := normalizeUnclassifiedFailure(refusal); got != refusal {
		t.Fatalf("normalizeUnclassifiedFailure(%v) = %v, want the error untouched", refusal, got)
	}
}

// The normalization is narrow on purpose: a vendor sentence that merely
// mentions a network word must not be promoted, or a 400 would be replayed
// across every platform.
func TestTransportNormalizationIgnoresVendorProse(t *testing.T) {
	for _, msg := range []string{
		"alpha: HTTP 400: the model is not available, connection settings are unchanged",
		"alpha: HTTP 429: too many requests",
		"alpha: HTTP 402: insufficient credit",
	} {
		err := fmt.Errorf("%s", msg)
		if got := normalizeUnclassifiedFailure(err); got != err {
			t.Errorf("normalizeUnclassifiedFailure(%q) was promoted to %v", msg, got)
		}
	}
}

// Real transport spellings, however, must be promoted so the chain can move on.
func TestTransportNormalizationPromotesTransportFaults(t *testing.T) {
	for _, msg := range []string{
		`Post "https://x/chat/completions": EOF`,
		"read tcp 10.0.0.1:5000->1.2.3.4:443: connection reset by peer",
		"dial tcp: lookup xiaohuanxiong.com: no such host",
		"alpha: request failed: net/http: TLS handshake timeout",
	} {
		err := fmt.Errorf("%s", msg)
		f, ok := core.AsFailure(normalizeUnclassifiedFailure(err))
		if !ok {
			t.Errorf("%q was not promoted to a classified failure", msg)
			continue
		}
		if f.Kind != core.FailureUpstream {
			t.Errorf("%q became %q, want %q", msg, f.Kind, core.FailureUpstream)
		}
		if !core.Retryable(f.Kind) {
			t.Errorf("%q is not retryable, so the chain would still stop", msg)
		}
	}
}

// A module whose client-error bucket swallows 413 lands on FailureOther, which
// the decision table treats as terminal.  That is the exact live failure mode:
// three 413s from openai-compat providers, none of them ever trying a sibling.
// The gateway promotes it to a retryable upstream failure, keeping the status.
func TestAnUnclassified413StillFailsOver(t *testing.T) {
	// The shape openai-compat produced before the module gained its own kind.
	refusal := core.Fail("hcnsec", "key-629e", core.FailureOther, http.StatusRequestEntityTooLarge,
		errors.New(`openai-compat: chat (HTTP 413): openai_error`))

	got := normalizeUnclassifiedFailure(refusal)
	f, ok := core.AsFailure(got)
	if !ok {
		t.Fatalf("normalizeUnclassifiedFailure = %v, want a classified failure", got)
	}
	if f.Kind != core.FailureUpstream {
		t.Fatalf("kind = %q, want %q so the chain can move on", f.Kind, core.FailureUpstream)
	}
	if !core.Retryable(f.Kind) {
		t.Fatal("the promoted 413 is not retryable, so the routing loop would still stop")
	}
	if f.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want the vendor's 413 preserved", f.Status)
	}
	if f.Account != "key-629e" {
		t.Fatalf("account = %q, want the blamed credential kept", f.Account)
	}
}

// The promotion is scoped to 413.  A 400 must keep its terminal FailureOther, or
// the same rejected body would be replayed on every platform in the chain.
func TestAnUnclassified400StaysTerminal(t *testing.T) {
	refusal := core.Fail("opencode", "acct", core.FailureOther, http.StatusBadRequest,
		errors.New("opencode: invalid request"))

	got := normalizeUnclassifiedFailure(refusal)
	f, ok := core.AsFailure(got)
	if !ok {
		t.Fatalf("normalizeUnclassifiedFailure = %v, want a classified failure", got)
	}
	if f.Kind != core.FailureOther {
		t.Fatalf("kind = %q, want %q untouched", f.Kind, core.FailureOther)
	}
	if core.Retryable(f.Kind) || core.NextPlatformOn(f.Kind) {
		t.Fatal("a 400 was made retryable, which would amplify it across every platform")
	}
}
