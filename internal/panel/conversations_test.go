package panel

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// fakeBinderClient adds the ConversationBinder capability on top of
// fakeAccountClient, so the panel's bind route has a module to talk to.
type fakeBinderClient struct {
	*fakeAccountClient

	mu      sync.Mutex
	entries map[string]string
	// usable is the module's own answer to "could this account serve right
	// now".  A module whose account is parked reports the binding as absent
	// rather than handing back an id it will not use.
	usable map[string]bool
}

func binderClient(name string, accounts ...core.AccountRecord) *fakeBinderClient {
	return &fakeBinderClient{
		fakeAccountClient: &fakeAccountClient{
			fakeClient: &fakeClient{name: name},
			accounts:   accounts,
		},
		entries: map[string]string{},
		usable:  map[string]bool{},
	}
}

func (f *fakeBinderClient) BindConversation(key, account string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[key] = account
}

func (f *fakeBinderClient) UnbindConversation(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.entries[key]; !ok {
		return false
	}
	delete(f.entries, key)
	return true
}

func (f *fakeBinderClient) ConversationAccount(key, _ string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.entries[key]
	if !ok {
		return "", false
	}
	if usable, decided := f.usable[id]; decided && !usable {
		return "", false
	}
	return id, true
}

func (f *fakeBinderClient) bound(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.entries[key]
	return id, ok
}

// allow marks one account as currently servable, which is what a module's
// usability check consults before it hands a binding back.
func (f *fakeBinderClient) allow(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usable[id] = true
}

func (f *fakeBinderClient) deny(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usable[id] = false
}

func conversationPanel(t *testing.T, clients ...core.Client) http.Handler {
	t.Helper()
	return New(Options{
		Registry: registryOf(clients...),
		Version:  "test",
		Listen:   "127.0.0.1:0",
		Started:  time.Now(),
	})
}

// TestConversationsBindMintsAKeyAndReadsItBack: the chat tab must never have to
// invent a stickiness key, and the panel must report what the module actually
// holds rather than what it was asked for.
func TestConversationsBindMintsAKeyAndReadsItBack(t *testing.T) {
	c := binderClient("verbs", core.AccountRecord{ID: "A1", Label: "一号"})
	c.allow("A1")
	h := conversationPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/verbs/conversations", `{"account":"A1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)
	key, _ := body["key"].(string)
	if key == "" {
		t.Fatalf("no key was minted: %s", rec.Body.String())
	}
	if body["account"] != "A1" || body["requested"] != "A1" || body["bound"] != true {
		t.Fatalf("bind body = %s", rec.Body.String())
	}
	if id, ok := c.bound(key); !ok || id != "A1" {
		t.Fatalf("module holds (%q,%v), want A1", id, ok)
	}

	read := hitRoute(t, h, http.MethodGet, "/panel/api/clients/verbs/conversations?key="+key, "")
	if read.Code != http.StatusOK {
		t.Fatalf("read status = %d, body = %s", read.Code, read.Body.String())
	}
	if got := decodeMap(t, read); got["account"] != "A1" || got["bound"] != true {
		t.Fatalf("read body = %s", read.Body.String())
	}
}

// TestConversationsBindHonoursACallerSuppliedKey: a caller that already has a
// key (a browser tab resuming a session) keeps it.
func TestConversationsBindHonoursACallerSuppliedKey(t *testing.T) {
	c := binderClient("verbs", core.AccountRecord{ID: "A1"})
	c.allow("A1")
	h := conversationPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/verbs/conversations", `{"account":"A1","key":"tab-7"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeMap(t, rec); got["key"] != "tab-7" {
		t.Fatalf("key = %v, want tab-7", got["key"])
	}
	if id, ok := c.bound("tab-7"); !ok || id != "A1" {
		t.Fatalf("module holds (%q,%v), want A1", id, ok)
	}
}

// TestConversationsRefuseAnUnknownAccount is the honesty case that matters: a
// module drops a binding whose account it does not have, so accepting the bind
// would let the operator believe the chosen account answered when a different
// one did.
func TestConversationsRefuseAnUnknownAccount(t *testing.T) {
	c := binderClient("verbs", core.AccountRecord{ID: "A1"})
	h := conversationPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/verbs/conversations", `{"account":"ghost"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("a refused bind left %d entries behind", n)
	}
}

// TestConversationsReportAnUnusableBindingAsUnbound: a parked credential must
// not come back as a successful pin, because the next chat would silently be
// served by another account.
func TestConversationsReportAnUnusableBindingAsUnbound(t *testing.T) {
	c := binderClient("verbs", core.AccountRecord{ID: "A1"})
	c.deny("A1")
	h := conversationPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/verbs/conversations", `{"account":"A1","key":"k1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeMap(t, rec)
	if got["bound"] != false || got["account"] != "" {
		t.Fatalf("unusable binding reported as %s", rec.Body.String())
	}
	// The requested id is still echoed, so the operator can see what they
	// asked for next to the fact that it is not being honoured.
	if got["requested"] != "A1" {
		t.Fatalf("requested = %v, want A1", got["requested"])
	}
}

// TestConversationsUnbindForgetsTheKey: the tab drops its key when the operator
// stops testing, and a second unbind honestly reports there was nothing left.
func TestConversationsUnbindForgetsTheKey(t *testing.T) {
	c := binderClient("verbs", core.AccountRecord{ID: "A1"})
	c.allow("A1")
	h := conversationPanel(t, c)

	hitRoute(t, h, http.MethodPost, "/panel/api/clients/verbs/conversations", `{"account":"A1","key":"k1"}`)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/verbs/conversations/unbind", `{"key":"k1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeMap(t, rec); got["forgotten"] != true {
		t.Fatalf("first unbind = %s", rec.Body.String())
	}
	if _, ok := c.bound("k1"); ok {
		t.Fatal("the binding survived the unbind")
	}

	again := hitRoute(t, h, http.MethodPost, "/panel/api/clients/verbs/conversations/unbind", `{"key":"k1"}`)
	if got := decodeMap(t, again); got["forgotten"] != false {
		t.Fatalf("second unbind = %s", again.Body.String())
	}
}

// TestConversationsWithoutTheCapabilityIs501: seven of the fourteen modules have
// no stickiness table, and the panel must say so instead of offering a dropdown
// whose selection is ignored.
func TestConversationsWithoutTheCapabilityIs501(t *testing.T) {
	h := conversationPanel(t, &fakeClient{name: "plain"})

	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/panel/api/clients/plain/conversations", `{"account":"A1"}`},
		{http.MethodGet, "/panel/api/clients/plain/conversations?key=k1", ""},
		{http.MethodPost, "/panel/api/clients/plain/conversations/unbind", `{"key":"k1"}`},
	} {
		rec := hitRoute(t, h, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s = %d, want 501 (body %s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestConversationsMethodGuards(t *testing.T) {
	c := binderClient("verbs", core.AccountRecord{ID: "A1"})
	c.allow("A1")
	h := conversationPanel(t, c)

	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"wrong method on the collection", http.MethodDelete, "/panel/api/clients/verbs/conversations", "", http.StatusMethodNotAllowed},
		{"bind without an account", http.MethodPost, "/panel/api/clients/verbs/conversations", `{}`, http.StatusBadRequest},
		{"bind with a bad body", http.MethodPost, "/panel/api/clients/verbs/conversations", `{`, http.StatusBadRequest},
		{"read without a key", http.MethodGet, "/panel/api/clients/verbs/conversations", "", http.StatusBadRequest},
		{"unbind without a key", http.MethodPost, "/panel/api/clients/verbs/conversations/unbind", `{}`, http.StatusBadRequest},
		{"unbind with the wrong method", http.MethodGet, "/panel/api/clients/verbs/conversations/unbind?key=k1", "", http.StatusMethodNotAllowed},
	} {
		rec := hitRoute(t, h, tc.method, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s: %s %s = %d, want %d (body %s)", tc.name, tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
}

// TestConversationsSurviveAModuleThatCannotListAccounts: accountExists answers
// "open" for a module without an AccountManager, so the bind proceeds rather
// than reporting a 404 the panel cannot justify.
func TestConversationsSurviveAModuleThatCannotListAccounts(t *testing.T) {
	c := &bareBinderClient{entries: map[string]string{}}
	h := conversationPanel(t, c)

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/bare/conversations", `{"account":"anything","key":"k1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeMap(t, rec); got["bound"] != true || got["account"] != "anything" {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// bareBinderClient is a module with stickiness but no account table.
type bareBinderClient struct {
	entries map[string]string
}

func (b *bareBinderClient) Name() string { return "bare" }

func (b *bareBinderClient) Models(context.Context) ([]core.Model, error) { return nil, nil }

func (b *bareBinderClient) Chat(context.Context, *core.ChatRequest) (core.Stream, error) {
	return nil, core.ErrUnsupported
}

func (b *bareBinderClient) Status(context.Context) core.Status { return core.Status{} }

func (b *bareBinderClient) BindConversation(key, account string) { b.entries[key] = account }

func (b *bareBinderClient) UnbindConversation(key string) bool {
	if _, ok := b.entries[key]; !ok {
		return false
	}
	delete(b.entries, key)
	return true
}

func (b *bareBinderClient) ConversationAccount(key, _ string) (string, bool) {
	id, ok := b.entries[key]
	return id, ok
}

// TestCapabilitiesReportConversations: the frontend hides the per-account test
// from this flag, so it has to be lit by the assertion and by nothing else.
func TestCapabilitiesReportConversations(t *testing.T) {
	withBinder := core.CapabilitiesOf(context.Background(), binderClient("verbs"))
	if !withBinder.Conversations {
		t.Error("a ConversationBinder did not light conversations")
	}
	without := core.CapabilitiesOf(context.Background(), &fakeClient{name: "plain"})
	if without.Conversations {
		t.Error("a module without a stickiness table reported conversations")
	}
}
