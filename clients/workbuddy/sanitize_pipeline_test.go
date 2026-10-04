package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
)

// The tests in sanitize_test.go exercise sanitizeText in isolation.  This file
// locks the wiring instead: the scrubber must actually run on the outbound body
// when the switch is on, and must leave the body alone when it is off.
//
// Expectations are derived from sanitizeText itself rather than hard-coded, so
// this test keeps its meaning even if a rewrite pair is ever re-tuned.

func TestPrepareBodyRunsTheSanitizerWhenAsked(t *testing.T) {
	wantIdentity := sanitizeText(ccIdentity)
	if wantIdentity == ccIdentity {
		t.Fatalf("fixture is not a fingerprint: %q", ccIdentity)
	}

	const billingHeader = "x-anthropic-billing-" + "header"
	wantHeader := sanitizeText(billingHeader)
	if wantHeader == billingHeader {
		t.Fatalf("fixture is not a fingerprint: %q", billingHeader)
	}

	kvKey := "cc_" + "entrypoint="
	userText := billingHeader + ": deadbeef; " + kvKey + "cli; keep me"

	raw, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4",
		"messages": []any{
			map[string]any{"role": "system", "content": ccIdentity},
			map[string]any{"role": "user", "content": userText},
		},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	scrubbed := string(prepareBody(raw, true, nil, nil))
	if strings.Contains(scrubbed, ccIdentity) {
		t.Errorf("identity survived the scrubbing pipeline: %s", scrubbed)
	}
	if !strings.Contains(scrubbed, wantIdentity) {
		t.Errorf("identity was not rewritten in place: %s", scrubbed)
	}
	if strings.Contains(scrubbed, billingHeader) {
		t.Errorf("billing header survived the scrubbing pipeline: %s", scrubbed)
	}
	if strings.Contains(scrubbed, kvKey) {
		t.Errorf("cc_ key/value survived the scrubbing pipeline: %s", scrubbed)
	}
	if !strings.Contains(scrubbed, "keep me") {
		t.Errorf("ordinary user text was dropped: %s", scrubbed)
	}

	kept := string(prepareBody(raw, false, nil, nil))
	if !strings.Contains(kept, ccIdentity) {
		t.Errorf("sanitize=false rewrote the identity anyway: %s", kept)
	}
	if !strings.Contains(kept, billingHeader) {
		t.Errorf("sanitize=false stripped the billing header anyway: %s", kept)
	}
	if !strings.Contains(kept, kvKey) {
		t.Errorf("sanitize=false stripped the cc_ key/value anyway: %s", kept)
	}
}

// The scrubber runs last, after the shape rewrites, so turning it on must not
// disturb steps 1-9 (stream, max_tokens, roles).
func TestTheSanitizerDoesNotDisturbTheShape(t *testing.T) {
	src := `{"model":"claude-sonnet-4","max_completion_tokens":4096,"messages":[` +
		`{"role":"developer","content":"hello there"}]}`

	out := prepareBody([]byte(src), true, nil, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("pipeline produced invalid JSON: %v (%s)", err, out)
	}
	if obj["stream"] != true {
		t.Errorf("step 1 lost: stream=%v", obj["stream"])
	}
	if obj["max_completion_tokens"] != nil || obj["max_tokens"] == nil {
		t.Errorf("step 2 lost: max_completion_tokens=%v max_tokens=%v",
			obj["max_completion_tokens"], obj["max_tokens"])
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("step 7 lost: messages=%v", obj["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("step 5 lost: role=%v", first["role"])
	}
}

func TestSanitizeDefaultsOn(t *testing.T) {
	if !(config{}).sanitizeFingerprints() {
		t.Error("body scrubbing must default on, matching the reference")
	}
	off := false
	if (config{SanitizeFingerprints: &off}).sanitizeFingerprints() {
		t.Error("sanitize_fingerprints:false must turn the scrubber off")
	}
	on := true
	if !(config{SanitizeFingerprints: &on}).sanitizeFingerprints() {
		t.Error("sanitize_fingerprints:true must keep the scrubber on")
	}
}

// The switch lives on Upstream, so this is the seam that decides whether
// production traffic is actually scrubbed.
func TestTheUpstreamSanitizeSwitchReachesTheBody(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4",
		"messages": []any{map[string]any{"role": "system", "content": ccIdentity}},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	on := &Upstream{Sanitize: true}
	if got := string(on.prepareBody(raw, "cn", "", "")); strings.Contains(got, ccIdentity) {
		t.Errorf("Sanitize=true did not scrub: %s", got)
	}
	off := &Upstream{Sanitize: false}
	if got := string(off.prepareBody(raw, "cn", "", "")); !strings.Contains(got, ccIdentity) {
		t.Errorf("Sanitize=false scrubbed anyway: %s", got)
	}
}
