package workbuddy

import (
	"encoding/hex"
	"strings"
	"testing"

	"client2api/internal/core"
)

func sidUser(text string) core.Message {
	return core.Message{Role: "user", Content: text}
}

func sidParts(texts ...string) []core.ContentPart {
	out := make([]core.ContentPart, 0, len(texts))
	for _, t := range texts {
		out = append(out, core.ContentPart{Type: "text", Text: t})
	}
	return out
}

func sidRequest(msgs ...core.Message) *core.ChatRequest {
	return &core.ChatRequest{Messages: msgs}
}

func assertHex32(t *testing.T, what, id string) {
	t.Helper()
	if len(id) != 32 {
		t.Fatalf("%s = %q, want 32 hex characters", what, id)
	}
	if _, err := hex.DecodeString(id); err != nil {
		t.Fatalf("%s = %q, want hex: %v", what, id, err)
	}
}

func TestWorkbuddyDerivedIDsAreStableAndDistinct(t *testing.T) {
	a := deriveID("session-a")
	b := deriveID("session-a")
	c := deriveID("session-b")

	assertHex32(t, "deriveID(session-a)", a)
	if a != b {
		t.Fatalf("deriveID is not deterministic: %q vs %q", a, b)
	}
	if a == c {
		t.Fatalf("deriveID collapsed two keys onto %q", a)
	}
}

func TestWorkbuddyKeyedIDsDegradeToRandomWhenTheKeyIsEmpty(t *testing.T) {
	// An empty key names no session and no turn, so there is nothing to
	// aggregate: each call has to be a fresh value rather than a shared one.
	first := requestIDForKey("")
	second := requestIDForKey("")
	assertHex32(t, "requestIDForKey(\"\")", first)
	if first == second {
		t.Fatalf("requestIDForKey(\"\") reused %q; an empty key must not aggregate", first)
	}

	third := turnRequestID("")
	fourth := turnRequestID("")
	assertHex32(t, "turnRequestID(\"\")", third)
	if third == fourth {
		t.Fatalf("turnRequestID(\"\") reused %q; an empty key must not aggregate", third)
	}
}

func TestWorkbuddyKeyedIDsAreDerivedWhenTheKeyIsPresent(t *testing.T) {
	if got, want := requestIDForKey("conv-1"), deriveID("conv-1"); got != want {
		t.Fatalf("requestIDForKey(conv-1) = %q, want %q", got, want)
	}
	if got, want := turnRequestID("u0:hi"), deriveID("u0:hi"); got != want {
		t.Fatalf("turnRequestID(u0:hi) = %q, want %q", got, want)
	}
}

func TestWorkbuddyTurnKeyUsesTheLastUserMessage(t *testing.T) {
	req := sidRequest(
		sidUser("first"),
		core.Message{Role: "assistant", Content: "reply"},
		sidUser("second"),
	)
	key := turnKey(req)
	if !strings.HasPrefix(key, "u2:") {
		t.Fatalf("turnKey = %q, want it to be keyed on the last user message (index 2)", key)
	}
	if !strings.Contains(key, "second") {
		t.Fatalf("turnKey = %q, want it to carry the last user message's content", key)
	}
}

func TestWorkbuddyTurnKeyMovesWhenTheTurnMoves(t *testing.T) {
	// The same text at a different position is a different turn: the whole
	// point of keying on position is that a growing history does not keep
	// re-using the previous turn's aggregation id.
	one := turnKey(sidRequest(sidUser("same")))
	two := turnKey(sidRequest(sidUser("same"), core.Message{Role: "assistant", Content: "x"}, sidUser("same")))
	if one == "" || two == "" {
		t.Fatalf("turnKey returned an empty key: %q / %q", one, two)
	}
	if one == two {
		t.Fatalf("turnKey collapsed two different positions onto %q", one)
	}
}

func TestWorkbuddyTurnKeyStopsAtAnUnsignableUserMessage(t *testing.T) {
	// Scanning further back would make the key drift from step to step as the
	// history grows, so the scan must stop at the first user message found from
	// the end even when it has nothing to sign.
	req := sidRequest(
		sidUser("signable"),
		core.Message{Role: "assistant", Content: "reply"},
		sidUser(""),
	)
	if key := turnKey(req); key != "" {
		t.Fatalf("turnKey = %q, want \"\" — it must not fall back to an earlier user message", key)
	}
}

func TestWorkbuddyTurnKeyIsEmptyWithoutAUserMessage(t *testing.T) {
	if key := turnKey(sidRequest(core.Message{Role: "system", Content: "be nice"})); key != "" {
		t.Fatalf("turnKey = %q, want \"\"", key)
	}
	if key := turnKey(sidRequest()); key != "" {
		t.Fatalf("turnKey(no messages) = %q, want \"\"", key)
	}
	if key := turnKey(nil); key != "" {
		t.Fatalf("turnKey(nil) = %q, want \"\"", key)
	}
}

func TestWorkbuddyContentSignatureAgreesAcrossBothContentForms(t *testing.T) {
	// A caller that sends its text as a string and one that sends the same text
	// as an array of one text part must land on the same aggregation id;
	// otherwise the id would change the moment a client switched spelling.
	asString := core.Message{Role: "user", Content: "hello there"}
	asParts := core.Message{Role: "user", Content: "hello there", Parts: sidParts("hello there")}
	if a, b := contentSignature(asString), contentSignature(asParts); a != b {
		t.Fatalf("contentSignature diverged: string %q vs parts %q", a, b)
	}

	split := core.Message{Role: "user", Content: "hello there", Parts: sidParts("hello ", "there")}
	if a, b := contentSignature(asString), contentSignature(split); a != b {
		t.Fatalf("contentSignature diverged: string %q vs split parts %q", a, b)
	}
}

func TestWorkbuddyContentSignatureDigestsNonTextParts(t *testing.T) {
	msg := core.Message{
		Role:  "user",
		Parts: []core.ContentPart{{Type: "image_url", ImageURL: "data:image/png;base64,AAAA", Detail: "high"}},
	}
	sig := contentSignature(msg)
	if sig == "" {
		t.Fatal("contentSignature dropped an image-only turn; it must still be aggregatable")
	}
	if strings.Contains(sig, "AAAA") {
		t.Fatalf("contentSignature leaked the raw payload: %q", sig)
	}
	if again := contentSignature(msg); again != sig {
		t.Fatalf("contentSignature is not stable: %q vs %q", sig, again)
	}

	other := core.Message{
		Role:  "user",
		Parts: []core.ContentPart{{Type: "image_url", ImageURL: "data:image/png;base64,BBBB", Detail: "high"}},
	}
	if contentSignature(other) == sig {
		t.Fatal("contentSignature gave two different images the same digest")
	}
}

func TestWorkbuddyContentSignatureKeepsTheTextAroundAnImage(t *testing.T) {
	msg := core.Message{
		Role: "user",
		Parts: []core.ContentPart{
			{Type: "text", Text: "what is this"},
			{Type: "image_url", ImageURL: "data:image/png;base64,AAAA"},
		},
	}
	sig := contentSignature(msg)
	if !strings.Contains(sig, "what is this") {
		t.Fatalf("contentSignature = %q, want it to keep the surrounding text", sig)
	}
}

func TestWorkbuddyConversationRequestIDPrefersTheCallersOwnID(t *testing.T) {
	req := sidRequest(sidUser("hi"))
	if got := conversationRequestID("  caller-id  ", "session-a", req); got != "caller-id" {
		t.Fatalf("conversationRequestID = %q, want the caller's own id", got)
	}
}

func TestWorkbuddyConversationRequestIDSaltsTheTurnKeyWithTheSession(t *testing.T) {
	req := sidRequest(sidUser("hi"))
	tk := turnKey(req)

	got := conversationRequestID("", "session-a", req)
	if want := deriveID("session-a:" + tk); got != want {
		t.Fatalf("conversationRequestID = %q, want %q", got, want)
	}

	// Identical turn text in two different conversations must not collide.
	if other := conversationRequestID("", "session-b", req); other == got {
		t.Fatalf("two sessions shared the aggregation id %q", got)
	}
}

func TestWorkbuddyConversationRequestIDFallsBackInOrder(t *testing.T) {
	req := sidRequest(sidUser("hi"))
	tk := turnKey(req)

	if got, want := conversationRequestID("", "", req), deriveID(tk); got != want {
		t.Fatalf("no session key: got %q, want the bare turn key %q", got, want)
	}

	// No signable turn at all: the session-level id is the leftover case.
	headless := sidRequest(core.Message{Role: "assistant", Content: "reply"})
	if got, want := conversationRequestID("", "session-a", headless), deriveID("session-a"); got != want {
		t.Fatalf("no turn key: got %q, want the session key %q", got, want)
	}

	// Neither: a request-scoped random value, never a shared constant.
	got := conversationRequestID("", "", headless)
	assertHex32(t, "conversationRequestID(no keys)", got)
	if again := conversationRequestID("", "", headless); again == got {
		t.Fatalf("an unscoped request reused the aggregation id %q", got)
	}
}

func TestWorkbuddyOneTurnProducesOneAggregationID(t *testing.T) {
	// This is the whole point of the file: every attempt of one user submit —
	// rotation, retry, downgrade — must carry the same id, and a new user
	// message must carry a new one.
	req := sidRequest(sidUser("do the thing"))
	first := conversationRequestID("", "session-a", req)
	for i := 0; i < 3; i++ {
		if got := conversationRequestID("", "session-a", req); got != first {
			t.Fatalf("attempt %d carried %q, want the turn's id %q", i, got, first)
		}
	}

	next := sidRequest(sidUser("do the thing"), core.Message{Role: "assistant", Content: "done"}, sidUser("now the other thing"))
	if got := conversationRequestID("", "session-a", next); got == first {
		t.Fatalf("a new user message reused the previous turn's id %q", got)
	}
}
