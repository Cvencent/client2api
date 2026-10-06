package openaicompat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"client2api/internal/core"
)

func newTestClient(t *testing.T, dataDir string) *Client {
	t.Helper()
	raw, err := New(core.Deps{DataDir: dataDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := raw.(*Client)
	c.ensure()
	return c
}

func TestAddAccountPersistsAndRoutes(t *testing.T) {
	dir := t.TempDir()
	c := newTestClient(t, dir)
	ctx := context.Background()

	rec, err := c.AddAccount(ctx, core.AccountSpec{
		Fields: map[string]string{
			"provider": "groq",
			"api_key":  "gsk_panel",
			"models":   "llama-3.3-70b-versatile, openai/gpt-oss-120b",
		},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec.ID != "groq" {
		t.Fatalf("record ID = %q, want groq", rec.ID)
	}

	list, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(list) != 1 || list[0].ID != "groq" {
		t.Fatalf("Accounts = %+v", list)
	}

	// The model must now resolve to this provider.
	prov, model, ok := c.providerFor("groq/llama-3.3-70b-versatile")
	if !ok {
		t.Fatal("providerFor failed after AddAccount")
	}
	if prov.APIKey != "gsk_panel" || model != "llama-3.3-70b-versatile" {
		t.Fatalf("resolved prov=%+v model=%q", prov, model)
	}

	// A fresh client over the same data dir must see the stored account.
	c2 := newTestClient(t, dir)
	prov2, _, ok := c2.providerFor("groq/openai/gpt-oss-120b")
	if !ok {
		t.Fatal("stored account not reloaded")
	}
	if prov2.APIKey != "gsk_panel" {
		t.Fatalf("reloaded key = %q", prov2.APIKey)
	}
}

func TestAddAccountRejectsConfigSource(t *testing.T) {
	dir := t.TempDir()
	raw, _ := New(core.Deps{DataDir: dir, Config: json.RawMessage(`{
		"providers": [{"id": "groq", "api_key": "gsk_config"}]
	}`)})
	c := raw.(*Client)
	c.ensure()

	_, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"provider": "groq", "api_key": "gsk_panel"},
	})
	if err == nil {
		t.Fatal("expected AddAccount to refuse a config-sourced provider")
	}
}

func TestRemoveAccountOnlyRemovesPanelRows(t *testing.T) {
	dir := t.TempDir()
	c := newTestClient(t, dir)
	ctx := context.Background()
	if _, err := c.AddAccount(ctx, core.AccountSpec{
		Fields: map[string]string{"provider": "cerebras", "api_key": "csk_panel"},
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := c.RemoveAccount(ctx, "cerebras"); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if _, _, ok := c.providerFor("cerebras/gpt-oss-120b"); ok {
		t.Fatal("provider should be gone after RemoveAccount")
	}
	if err := c.RemoveAccount(ctx, "cerebras"); err == nil {
		t.Fatal("expected error removing an unknown account")
	}
}

func TestSetAccountEnabledPersists(t *testing.T) {
	dir := t.TempDir()
	c := newTestClient(t, dir)
	ctx := context.Background()
	if _, err := c.AddAccount(ctx, core.AccountSpec{
		Fields: map[string]string{"provider": "mistral", "api_key": "k"},
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := c.SetAccountEnabled(ctx, "mistral", false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	c2 := newTestClient(t, dir)
	prov, _, ok := c2.providerFor("mistral/mistral-large-latest")
	if !ok {
		t.Fatal("provider missing after reload")
	}
	if !prov.Disabled {
		t.Fatal("disabled flag was not persisted")
	}
}

func TestAccountFieldsDeclareProviderAndKey(t *testing.T) {
	c := newTestClient(t, t.TempDir())
	fields := c.AccountFields(context.Background())
	keys := map[string]bool{}
	for _, f := range fields {
		keys[f.Key] = true
	}
	for _, want := range []string{"provider", "api_key"} {
		if !keys[want] {
			t.Errorf("AccountFields missing %q", want)
		}
	}
}

// The provider id is a routing prefix, not a closed enum: arbitrary
// OpenAI-compatible sources are explicitly supported. The form must keep the
// known ids as suggestions while still allowing an operator to type one.
func TestProviderFieldAcceptsCustomIDs(t *testing.T) {
	c := newTestClient(t, t.TempDir())
	for _, f := range c.AccountFields(context.Background()) {
		if f.Key != "provider" {
			continue
		}
		if f.Type == "select" {
			t.Fatal("provider is a select field, so custom provider ids cannot be entered")
		}
		if len(f.Options) == 0 {
			t.Fatal("provider has no built-in suggestions")
		}
		return
	}
	t.Fatal("provider field not found")
}

// The add form's provider picker is only half the operator's job; the other
// half is getting a key from that vendor.  Every provider the field spec
// offers must therefore have a key page, or the panel's "go get a key" link
// silently disappears for an option the operator can still select.
func TestKeyPagesCoverEveryOfferedProvider(t *testing.T) {
	c := newTestClient(t, t.TempDir())
	ctx := context.Background()
	pages := c.KeyPageURLs(ctx)
	if len(pages) == 0 {
		t.Fatal("KeyPageURLs is empty, so the panel has nothing to link to")
	}
	var offered []string
	for _, f := range c.AccountFields(ctx) {
		if f.Key == "provider" {
			offered = f.Options
		}
	}
	if len(offered) == 0 {
		t.Fatal("the provider field offers no options")
	}
	for _, id := range offered {
		if strings.TrimSpace(pages[id]) == "" {
			t.Errorf("provider %q has no key page", id)
		}
	}
	for id, u := range pages {
		if !strings.HasPrefix(u, "https://") {
			t.Errorf("key page for %q is not an https URL: %q", id, u)
		}
	}
}
