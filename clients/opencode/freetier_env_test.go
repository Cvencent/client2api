package opencode

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

// ---------------------------------------------------------------------------
// The free-tier gate moved from the tool list alone to the tool list AND the
// system message's environment block.  These tests pin both halves, and the
// FreeUsageLimitError verdict that a refusal lands on.
// ---------------------------------------------------------------------------

// TestFreeChatBodyCarriesTheEnvBlock pins the second half of the gate: the
// free body's system message must carry the `<env>` block, whether or not the
// caller supplied a system message of their own.
func TestFreeChatBodyCarriesTheEnvBlock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		system  string
		wantSub string
	}{
		{"no caller system", "", "<env>"},
		{"caller system kept, block appended", "You are a pirate.", "You are a pirate."},
	} {
		req := chatRequest("space-bunny-free")
		if tc.system != "" {
			req.Messages = append([]core.Message{{Role: "system", Content: tc.system}}, req.Messages...)
		}
		raw, err := buildFreeChatBody(Config{}, req)
		if err != nil {
			t.Fatalf("%s: buildFreeChatBody = %v", tc.name, err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: the body is not JSON: %v", tc.name, err)
		}
		system := systemContentOf(t, got)
		if !strings.Contains(system, "<env>") || !strings.Contains(system, "Working directory:") {
			t.Fatalf("%s: the free body carries no env block: %q", tc.name, system)
		}
		if tc.wantSub != "" && !strings.Contains(system, tc.wantSub) {
			t.Fatalf("%s: the caller's system message was dropped: %q", tc.name, system)
		}
	}
}

// TestPlainChatBodyCarriesNoEnvBlock covers the other side: a paid-model body
// must not grow a block the plain handshake never carried.
func TestPlainChatBodyCarriesNoEnvBlock(t *testing.T) {
	raw, err := buildChatBody(Config{}, chatRequest("gpt-5.1"))
	if err != nil {
		t.Fatalf("buildChatBody = %v", err)
	}
	if strings.Contains(string(raw), "<env>") {
		t.Fatalf("the plain body carries an env block: %s", raw[:min(300, len(raw))])
	}
}

// TestFreeEnvBlockSystemMessageComesFirst keeps the system message first: the
// vendor and every OpenAI-compatible backend expect the system turn up front.
func TestFreeEnvBlockSystemMessageComesFirst(t *testing.T) {
	req := chatRequest("space-bunny-free")
	req.Messages = append([]core.Message{{Role: "system", Content: "Be brief."}}, req.Messages...)
	raw, err := buildFreeChatBody(Config{}, req)
	if err != nil {
		t.Fatalf("buildFreeChatBody = %v", err)
	}
	sysIdx := strings.Index(string(raw), `"role":"system"`)
	if sysIdx == -1 {
		t.Fatalf("the body carries no system turn: %s", raw[:min(300, len(raw))])
	}
	userIdx := strings.Index(string(raw), `"role":"user"`)
	if userIdx == -1 || userIdx < sysIdx {
		t.Fatalf("the system turn is not before the user turn: system@%d user@%d", sysIdx, userIdx)
	}
}

// TestFreeUsageLimitErrorParksForQuotaCooldown is the verdict the gate's
// refusal lands on.  The message carries neither "rate limit" nor "quota" nor
// "free tier", so without the marker it fell to the 429 status fallback and
// the account retried every minute against a wall that does not move.
func TestFreeUsageLimitErrorParksForQuotaCooldown(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	acct, _ := c.pool.byID("opencode:a")

	body := []byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached. Free models work in OpenCode; subscribe to OpenCode Go to keep going"}}`)
	err := c.classifyHTTP("chat", acct, 429, body)
	if kind := core.FailureKindOf(err); kind != core.FailureQuota {
		t.Fatalf("kind = %q, want %q so the account parks instead of spinning", kind, core.FailureQuota)
	}
	after, _ := c.pool.byID("opencode:a")
	if after.CooldownKind != "quota" {
		t.Fatalf("CooldownKind = %q, want quota", after.CooldownKind)
	}
	if !after.Enabled {
		t.Fatal("the gate refusal disabled the account; it must only be parked")
	}
	if after.CooldownUntil == "" {
		t.Fatal("the gate refusal left no cooldown")
	}
	until, err := time.Parse(time.RFC3339, after.CooldownUntil)
	if err != nil {
		t.Fatalf("CooldownUntil %q is not RFC3339: %v", after.CooldownUntil, err)
	}
	// The hermetic clock is frozen at testNow, so the remaining park is
	// measured from there, not from the real time.
	if remain := until.Sub(testNow); remain < defaultQuotaCooldown-time.Minute {
		t.Fatalf("the gate refusal parked the account for %s, want about %s", remain, defaultQuotaCooldown)
	}
}

// TestFreeUsageLimitErrorWithoutEnvelopeStillParks covers the marker path: the
// prose alone (no error envelope) must reach the same verdict.
func TestFreeUsageLimitErrorWithoutEnvelopeStillParks(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	acct, _ := c.pool.byID("opencode:a")

	err := c.classifyHTTP("chat", acct, 429, []byte(`Free usage limit reached`))
	if kind := core.FailureKindOf(err); kind != core.FailureQuota {
		t.Fatalf("kind = %q, want %q", kind, core.FailureQuota)
	}
}

// TestTLSProfileDefaultsToBunAndFallsBack pins the config key: absent means
// bun, "none" means stock, an unknown name falls back to bun rather than
// silently changing the handshake.
func TestTLSProfileDefaultsToBunAndFallsBack(t *testing.T) {
	if got := (Config{}).normalize().TLSProfile; got != "bun" {
		t.Fatalf("absent tls_profile = %q, want bun", got)
	}
	if got := (Config{TLSProfile: "none"}).normalize().TLSProfile; got != "" {
		t.Fatalf("tls_profile=none = %q, want empty", got)
	}
	if got := (Config{TLSProfile: "bogus-profile"}).normalize().TLSProfile; got != "bun" {
		t.Fatalf("tls_profile=bogus = %q, want the bun fallback", got)
	}
	if got := (Config{TLSProfile: "Chrome"}).normalize().TLSProfile; got != "chrome" {
		t.Fatalf("tls_profile=Chrome = %q, want chrome", got)
	}
}

// TestBunProfileIsKnownAndEmittable keeps the default profile honest: the
// package must know the name and build a spec for it, or the module's default
// would silently fall back to the stock hello.
func TestBunProfileIsKnownAndEmittable(t *testing.T) {
	if !fingerprint.Known(fingerprint.ProfileBun) {
		t.Fatal("the fingerprint package does not know the bun profile")
	}
	if _, err := fingerprint.New(fingerprint.Options{Profile: fingerprint.ProfileBun, Protocol: fingerprint.ProtocolH1}); err != nil {
		t.Fatalf("fingerprint.New(bun, h1) = %v", err)
	}
	if !slices.Contains(fingerprint.Profiles(), "bun") {
		t.Fatalf("Profiles() does not list bun: %v", fingerprint.Profiles())
	}
}

// systemContentOf renders the first system turn's content as a string, so the
// block and the caller's own text can be searched for in one form.
func systemContentOf(t *testing.T, body map[string]any) string {
	t.Helper()
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("the body carries no messages: %v", body)
	}
	first, _ := msgs[0].(map[string]any)
	if first == nil || first["role"] != "system" {
		t.Fatalf("the first message is not the system turn: %v", msgs[0])
	}
	switch c := first["content"].(type) {
	case string:
		return c
	case []any:
		var out strings.Builder
		for _, part := range c {
			if m, ok := part.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					out.WriteString(text)
				}
			}
		}
		return out.String()
	default:
		t.Fatalf("the system content is neither a string nor an array: %T", first["content"])
		return ""
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
