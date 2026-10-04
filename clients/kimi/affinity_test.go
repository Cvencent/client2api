package kimi

// Offline tests for the ConversationBinder (affinity.go).
//
// kimi has no pool, so a request is served by one of exactly two identities: the
// panel's own login over HTTPS (webLoginID), or the CLI the runner resolves
// (cliLoginID, or an executable binding the operator imported).  These tests pin
// the three properties that make a binding worth having:
//
//  1. an account that cannot serve right now never keeps a conversation, so a
//     binding can never turn stickiness into a source of failures;
//  2. an empty conversation key is never a binding, so a request the caller did
//     not scope behaves exactly as it did before this feature existed;
//  3. a conversation pinned to the CLI account never consults the direct HTTPS
//     path at all -- and the control below proves the same fixture *would* have
//     consulted it without the pin.  That is the whole point: answering a pinned
//     turn from the other credential is the failure this exists to prevent.
//
// Everything here is hermetic: credentials are pointed at a temp directory, PATH
// is emptied, the "CLI" is a stub script, and the vendor transport is a recorder
// that never opens a socket.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// recordingTransport counts the requests the direct HTTPS path made and refuses
// them.  A count of zero is the assertion: a conversation pinned to the CLI must
// not reach the vendor's HTTPS endpoint at all.  Refusing rather than answering
// keeps the test honest -- if the pin is ignored the request still happens, and
// the count says so instead of the test silently passing on a stubbed 200.
type recordingTransport struct {
	mu   sync.Mutex
	urls []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.urls = append(r.urls, req.URL.String())
	r.mu.Unlock()
	return nil, errors.New("the affinity tests never talk to the vendor")
}

func (r *recordingTransport) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.urls)
}

// affinityClient is newClient plus a transport for the direct HTTPS path, which
// the shared harness cannot inject (it passes no Deps.HTTPClient).
func affinityClient(t *testing.T, cfg map[string]any, rt http.RoundTripper) (*Client, string) {
	t.Helper()
	isolateCredentials(t)
	noCLIOnPath(t)

	dir := t.TempDir()
	var raw json.RawMessage
	if cfg != nil {
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	deps := core.Deps{DataDir: dir, Config: raw, Logf: func(string, ...any) {}}
	if rt != nil {
		deps.HTTPClient = &http.Client{Transport: rt}
	}
	cl, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := cl.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", cl)
	}
	return c, dir
}

// seedWebToken stores a panel login the way the device flow does, so the direct
// HTTPS path has a credential to spend.  No expiry: the vendor is the authority
// on whether a token still works, and an expiry would invite a renewal.
func seedWebToken(t *testing.T, dir string) {
	t.Helper()
	body := `{"access_token":"tok-affinity-aaaaaaaaaaaaaaaa","token_type":"Bearer"}`
	if err := os.WriteFile(filepath.Join(dir, tokenFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// cliFixture is a one-line stub-CLI answer.
func cliFixture(text string) string {
	return `{"role":"assistant","content":"` + text + `"}` + "\n"
}

// streamText concatenates the text deltas of a drained stream.  (ndjson.go
// already owns the name deltaText for its own purpose.)
func streamText(events []core.Event) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Type == core.EventDelta {
			b.WriteString(ev.Delta)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// The capability and the table itself
// ---------------------------------------------------------------------------

func TestKimiAdvertisesTheConversationBinder(t *testing.T) {
	c, _ := affinityClient(t, map[string]any{"assume_logged_in": true}, nil)

	var binder core.ConversationBinder = c
	if binder == nil {
		t.Fatal("Client does not implement core.ConversationBinder")
	}
	if _, ok := core.AsConversationBinder(c); !ok {
		t.Fatal("AsConversationBinder said no")
	}
	if caps := core.CapabilitiesOf(context.Background(), c); !caps.Conversations {
		t.Errorf("capabilities.Conversations = false, want true (%+v)", caps)
	}
}

func TestKimiBindsAndResolvesTheCLIAccount(t *testing.T) {
	bin := stubEmitting(t, cliFixture("x"))
	c, _ := affinityClient(t, map[string]any{"binary": bin, "assume_logged_in": true}, nil)

	c.BindConversation("conv-cli", cliLoginID)
	got, ok := c.ConversationAccount("conv-cli", "kimi")
	if !ok || got != cliLoginID {
		t.Fatalf("ConversationAccount = %q,%v; want %q,true", got, ok, cliLoginID)
	}
	if n := c.affinity.Count(); n != 1 {
		t.Errorf("affinity count = %d, want 1", n)
	}
}

func TestKimiAnAccountThatCannotServeIsDropped(t *testing.T) {
	bin := stubEmitting(t, cliFixture("x"))
	c, _ := affinityClient(t, map[string]any{"binary": bin, "assume_logged_in": true}, nil)

	c.BindConversation("conv-nope", "not-an-account")
	if got, ok := c.ConversationAccount("conv-nope", "kimi"); ok {
		t.Fatalf("ConversationAccount = %q,true; want absent", got)
	}
	// Resolve must forget the row, not merely decline to return it, or the table
	// would grow a dead entry per abandoned conversation.
	if n := c.affinity.Count(); n != 0 {
		t.Errorf("affinity count = %d, want 0", n)
	}
}

func TestKimiADeadAccountLosesItsConversation(t *testing.T) {
	bin := stubEmitting(t, cliFixture("x"))
	c, _ := affinityClient(t, map[string]any{"binary": bin, "assume_logged_in": true}, nil)

	c.BindConversation("conv-dead", cliLoginID)
	if _, ok := c.ConversationAccount("conv-dead", "kimi"); !ok {
		t.Fatal("the binding was not honoured before the account died")
	}
	c.markDead(cliLoginID, causeNoCredential, "no credential was found")

	if got, ok := c.ConversationAccount("conv-dead", "kimi"); ok {
		t.Fatalf("ConversationAccount = %q,true after the account died; want absent", got)
	}
	if n := c.affinity.Count(); n != 0 {
		t.Errorf("affinity count = %d, want 0", n)
	}
}

func TestKimiTheWebLoginIsOnlyUsableWhileTheDirectPathIsOn(t *testing.T) {
	bin := stubEmitting(t, cliFixture("x"))

	// prefer_http defaults to true, so the panel login is a serving identity.
	on, dir := affinityClient(t, map[string]any{"binary": bin, "assume_logged_in": true}, nil)
	seedWebToken(t, dir)
	on.BindConversation("conv-web", webLoginID)
	if got, ok := on.ConversationAccount("conv-web", "kimi"); !ok || got != webLoginID {
		t.Fatalf("ConversationAccount = %q,%v; want %q,true", got, ok, webLoginID)
	}

	// With the direct path switched off the panel login cannot serve at all, so
	// the very same binding must not survive.
	off, offDir := affinityClient(t, map[string]any{
		"binary":           bin,
		"assume_logged_in": true,
		"prefer_http":      false,
	}, nil)
	seedWebToken(t, offDir)
	off.BindConversation("conv-web", webLoginID)
	if got, ok := off.ConversationAccount("conv-web", "kimi"); ok {
		t.Fatalf("ConversationAccount = %q,true with prefer_http=false; want absent", got)
	}
	if n := off.affinity.Count(); n != 0 {
		t.Errorf("affinity count = %d, want 0", n)
	}
}

func TestKimiUnbindForgetsTheBinding(t *testing.T) {
	bin := stubEmitting(t, cliFixture("x"))
	c, _ := affinityClient(t, map[string]any{"binary": bin, "assume_logged_in": true}, nil)

	c.BindConversation("conv-unbind", cliLoginID)
	if !c.UnbindConversation("conv-unbind") {
		t.Fatal("UnbindConversation reported nothing to forget")
	}
	if c.UnbindConversation("conv-unbind") {
		t.Error("UnbindConversation reported a second removal")
	}
	if _, ok := c.ConversationAccount("conv-unbind", "kimi"); ok {
		t.Error("the binding survived UnbindConversation")
	}
}

func TestKimiAnEmptyConversationKeyIsNeverBound(t *testing.T) {
	bin := stubEmitting(t, cliFixture("x"))
	c, _ := affinityClient(t, map[string]any{"binary": bin, "assume_logged_in": true}, nil)

	c.BindConversation("", cliLoginID)
	c.BindConversation("   ", cliLoginID)
	c.BindConversation("conv-blank", "")
	c.BindConversation("conv-blank", "   ")

	if n := c.affinity.Count(); n != 0 {
		t.Errorf("affinity count = %d, want 0", n)
	}
	if _, ok := c.ConversationAccount("", "kimi"); ok {
		t.Error("an empty key resolved to an account")
	}
	if _, ok := c.ConversationAccount("   ", "kimi"); ok {
		t.Error("a blank key resolved to an account")
	}
}

func TestKimiConversationKeyReadsTheSpellingsTheGatewaySends(t *testing.T) {
	cases := []struct {
		name string
		req  *core.ChatRequest
		want string
	}{
		{"conversation_id", &core.ChatRequest{Options: map[string]any{"conversation_id": "k1"}}, "k1"},
		{"conversationId", &core.ChatRequest{Options: map[string]any{"conversationId": "k2"}}, "k2"},
		{"prompt_cache_key", &core.ChatRequest{Options: map[string]any{"prompt_cache_key": "k3"}}, "k3"},
		{"user fallback", &core.ChatRequest{User: "u1"}, "u1"},
		{"trimmed", &core.ChatRequest{Options: map[string]any{"conversation_id": "  k4  "}}, "k4"},
		{"nothing", &core.ChatRequest{}, ""},
		{"nil request", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := conversationKey(tc.req); got != tc.want {
				t.Errorf("conversationKey = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKimiApplyLiveRetunesTheWindow(t *testing.T) {
	c, _ := affinityClient(t, map[string]any{"assume_logged_in": true}, nil)

	before := c.affinity.TTL()
	c.ApplyLive(core.LiveSettings{})
	if got := c.affinity.TTL(); got != before {
		t.Errorf("a zero LiveSettings changed the TTL: %s -> %s", before, got)
	}

	c.ApplyLive(core.LiveSettings{AffinityTTL: 7 * time.Minute, AffinityGCInterval: 90 * time.Second})
	if got := c.affinity.TTL(); got != 7*time.Minute {
		t.Errorf("TTL = %s, want 7m", got)
	}
	if got := c.affinity.GCInterval(); got != 90*time.Second {
		t.Errorf("GCInterval = %s, want 90s", got)
	}
}

// ---------------------------------------------------------------------------
// The path decision in Chat
// ---------------------------------------------------------------------------

// TestKimiAPinnedConversationNeverConsultsTheDirectPath is the reason this
// feature exists: the operator picked the CLI account, so the panel login must
// not be asked -- not even as a first try that would be answered from the wrong
// credential.
func TestKimiAPinnedConversationNeverConsultsTheDirectPath(t *testing.T) {
	bin := stubEmitting(t, cliFixture("from the CLI"))
	rt := &recordingTransport{}
	c, dir := affinityClient(t, map[string]any{"binary": bin, "assume_logged_in": true}, rt)
	seedWebToken(t, dir)

	req := userReq("kimi", "hi")
	req.Options = map[string]any{"conversation_id": "conv-pinned"}
	c.BindConversation("conv-pinned", cliLoginID)
	if got, ok := c.ConversationAccount("conv-pinned", "kimi"); !ok || got != cliLoginID {
		t.Fatalf("the pin did not resolve: %q,%v; want %q,true", got, ok, cliLoginID)
	}

	st, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer func() { _ = st.Close() }()

	events := recvAll(t, st)
	if n := rt.calls(); n != 0 {
		t.Fatalf("the direct HTTPS path was consulted %d time(s) for a conversation pinned to %s: %v",
			n, cliLoginID, rt.urls)
	}
	if got := streamText(events); got != "from the CLI" {
		t.Errorf("text = %q, want %q", got, "from the CLI")
	}
}

// TestKimiAnUnpinnedConversationTriesTheDirectPathFirst is the control.  Without
// it the test above would pass even if the direct path had been removed
// altogether, and the pin would look honoured for the wrong reason.
func TestKimiAnUnpinnedConversationTriesTheDirectPathFirst(t *testing.T) {
	bin := stubEmitting(t, cliFixture("from the CLI"))
	rt := &recordingTransport{}
	c, dir := affinityClient(t, map[string]any{"binary": bin, "assume_logged_in": true}, rt)
	seedWebToken(t, dir)

	req := userReq("kimi", "hi")
	req.Options = map[string]any{"conversation_id": "conv-unpinned"}

	st, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer func() { _ = st.Close() }()
	_ = recvAll(t, st)

	if rt.calls() == 0 {
		t.Fatal("the direct HTTPS path was never consulted, so the pinned test proves nothing")
	}
}

// TestKimiAPinnedWebConversationIsNotRetriedOnTheCLI covers the other direction:
// the panel login is the account the operator chose, so when it fails the turn
// fails.  Serving it from the CLI would answer the question with a credential the
// operator did not pick, which is exactly the silent substitution the panel's
// per-account test exists to rule out.
func TestKimiAPinnedWebConversationIsNotRetriedOnTheCLI(t *testing.T) {
	bin := stubEmitting(t, cliFixture("from the CLI"))
	rt := &recordingTransport{}
	c, dir := affinityClient(t, map[string]any{"binary": bin, "assume_logged_in": true}, rt)
	seedWebToken(t, dir)

	req := userReq("kimi", "hi")
	req.Options = map[string]any{"conversation_id": "conv-web-pinned"}
	c.BindConversation("conv-web-pinned", webLoginID)
	if got, ok := c.ConversationAccount("conv-web-pinned", "kimi"); !ok || got != webLoginID {
		t.Fatalf("the web pin did not resolve: %q,%v; want %q,true", got, ok, webLoginID)
	}

	st, err := c.Chat(context.Background(), req)
	if err == nil {
		_ = st.Close()
		t.Fatal("Chat succeeded; the pinned panel login failed, so this turn must not be answered by the CLI")
	}
	if !strings.Contains(err.Error(), webLoginID) {
		t.Errorf("Chat error = %v, want it to name the pinned account %q", err, webLoginID)
	}
	// The turn is refused because the account the operator picked failed, not
	// because anything is misconfigured -- so the vendor failure must survive
	// rather than being replaced by a configuration error.
	if !strings.Contains(err.Error(), "never talk to the vendor") {
		t.Errorf("Chat error = %v, want the direct-path failure preserved", err)
	}
	// Consulted exactly once: the pin suppresses the fallback, not the attempt.
	if n := rt.calls(); n != 1 {
		t.Errorf("the direct HTTPS path was consulted %d time(s), want exactly 1", n)
	}
}
