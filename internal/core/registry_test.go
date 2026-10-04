package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var errFakeNoChat = errors.New("this fixture cannot chat")

// fakeClient is the smallest thing that satisfies Client.  Resolve only ever
// touches Name and Models, so the other two methods are stubs.
type fakeClient struct {
	name   string
	models []Model
}

func (f *fakeClient) Name() string { return f.name }

func (f *fakeClient) Models(context.Context) ([]Model, error) { return f.models, nil }

func (f *fakeClient) Chat(context.Context, *ChatRequest) (Stream, error) {
	return nil, errFakeNoChat
}

func (f *fakeClient) Status(context.Context) Status { return Status{Name: f.name} }

func registryWith(t *testing.T, clients ...Client) *Registry {
	t.Helper()
	r := NewRegistry()
	for _, c := range clients {
		r.Add(c)
	}
	return r
}

func resolveOK(t *testing.T, r *Registry, model string) (string, string) {
	t.Helper()
	c, upstream, err := r.Resolve(context.Background(), model)
	if err != nil {
		t.Fatalf("Resolve(%q) returned an error: %v", model, err)
	}
	return c.Name(), upstream
}

// ---------------------------------------------------------------------------
// The documented forms
// ---------------------------------------------------------------------------

func TestResolveReadsAQualifiedIDAsModuleSlashModel(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "zcode", models: []Model{{ID: "GLM-5.3"}}})

	gotClient, gotModel := resolveOK(t, r, "zcode/GLM-5.3")
	if gotClient != "zcode" || gotModel != "GLM-5.3" {
		t.Fatalf("Resolve = (%q, %q), want (zcode, GLM-5.3)", gotClient, gotModel)
	}
}

func TestResolveFindsABareNameInOneCatalogue(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "zcode", models: []Model{{ID: "GLM-5.3"}}})

	gotClient, gotModel := resolveOK(t, r, "glm-5.3")
	if gotClient != "zcode" || gotModel != "GLM-5.3" {
		t.Fatalf("Resolve = (%q, %q), want (zcode, GLM-5.3)", gotClient, gotModel)
	}
}

func TestResolveAnAliasWinsOverTheSlashCut(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "trae", models: []Model{{ID: "custom_model_gpt-5"}}})
	r.AddAlias("gpt-4o", "trae/custom_model_gpt-5")

	gotClient, gotModel := resolveOK(t, r, "gpt-4o")
	if gotClient != "trae" || gotModel != "custom_model_gpt-5" {
		t.Fatalf("Resolve = (%q, %q), want (trae, custom_model_gpt-5)", gotClient, gotModel)
	}
}

// ---------------------------------------------------------------------------
// The case the slash cut used to swallow
//
// An upstream's own model id may contain a slash, so a request whose first
// segment names no module is not necessarily a typo.  Before the fall-through
// was added, every test in this block failed with
// `unknown client "cline-free" in model "cline-free/gemini-3.8-flash"`.
// ---------------------------------------------------------------------------

func TestResolveFindsAModelWhoseOwnIDContainsASlash(t *testing.T) {
	// cline really ships ids shaped like this (clients/cline/models.go).
	r := registryWith(t, &fakeClient{name: "cline", models: []Model{
		{ID: "stealth/space-bunny-alpha"},
		{ID: "cline-free/gemini-3.8-flash"},
	}})

	for _, model := range []string{"stealth/space-bunny-alpha", "cline-free/gemini-3.8-flash"} {
		gotClient, gotModel := resolveOK(t, r, model)
		if gotClient != "cline" || gotModel != model {
			t.Fatalf("Resolve(%q) = (%q, %q), want (cline, %q)", model, gotClient, gotModel, model)
		}
	}
}

func TestResolveStillAcceptsTheQualifiedFormOfASlashedID(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "cline", models: []Model{{ID: "cline-free/gemini-3.8-flash"}}})

	gotClient, gotModel := resolveOK(t, r, "cline/cline-free/gemini-3.8-flash")
	if gotClient != "cline" || gotModel != "cline-free/gemini-3.8-flash" {
		t.Fatalf("Resolve = (%q, %q), want (cline, cline-free/gemini-3.8-flash)", gotClient, gotModel)
	}
}

func TestResolveFindsAnUpstreamAuthorPrefix(t *testing.T) {
	// Every OpenRouter id looks like this: the author is part of the model id,
	// not a module name.
	r := registryWith(t, &fakeClient{name: "openrouter", models: []Model{
		{ID: "anthropic/claude-sonnet-4.5"},
		{ID: "google/gemini-3.8-flash"},
	}})

	gotClient, gotModel := resolveOK(t, r, "anthropic/claude-sonnet-4.5")
	if gotClient != "openrouter" || gotModel != "anthropic/claude-sonnet-4.5" {
		t.Fatalf("Resolve = (%q, %q), want (openrouter, anthropic/claude-sonnet-4.5)", gotClient, gotModel)
	}
}

// A module name is still the stronger reading.  If a module is literally named
// "anthropic", "anthropic/x" must reach it and not be treated as a catalogue id
// of some other module.
func TestResolvePrefersAModuleNameOverACatalogueMatch(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "openrouter", models: []Model{{ID: "anthropic/x"}}},
		&fakeClient{name: "anthropic", models: []Model{{ID: "x"}}},
	)

	gotClient, gotModel := resolveOK(t, r, "anthropic/x")
	if gotClient != "anthropic" || gotModel != "x" {
		t.Fatalf("Resolve = (%q, %q), want (anthropic, x)", gotClient, gotModel)
	}
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

func TestResolveNamesAnUnknownModuleWhenNothingElseMatches(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "zcode", models: []Model{{ID: "GLM-5.3"}}})

	_, _, err := r.Resolve(context.Background(), "nosuch/model")
	if err == nil {
		t.Fatal("Resolve accepted a model no module serves")
	}
	msg := err.Error()
	if !strings.Contains(msg, `unknown client "nosuch"`) {
		t.Fatalf("the error does not name the bad module: %q", msg)
	}
	if !strings.Contains(msg, "qualify it as <client>/nosuch/model") {
		t.Fatalf("the error does not say how to fix it: %q", msg)
	}
}

func TestResolveRefusesAnUnqualifiedModelNobodyServes(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "zcode", models: []Model{{ID: "GLM-5.3"}}})

	_, _, err := r.Resolve(context.Background(), "gpt-9")
	if err == nil {
		t.Fatal("Resolve accepted a model no module serves")
	}
	if !strings.Contains(err.Error(), "no client serves model") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A shared id no longer needs a tie-break when the caller named the platform:
// qualification pins one owner deterministically.
func TestResolveQualifiedIDPinsOneOfSeveralOwners(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "cline", models: []Model{{ID: "vendor/model"}}},
		&fakeClient{name: "openrouter", models: []Model{{ID: "vendor/model"}}},
	)

	gotClient, gotModel := resolveOK(t, r, "openrouter/vendor/model")
	if gotClient != "openrouter" || gotModel != "vendor/model" {
		t.Fatalf("Resolve = (%q, %q), want (openrouter, vendor/model)", gotClient, gotModel)
	}
}

func TestResolveRefusesAnEmptyModel(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "zcode"})

	for _, model := range []string{"", "   ", "\t\n"} {
		if _, _, err := r.Resolve(context.Background(), model); err == nil {
			t.Fatalf("Resolve(%q) accepted a blank model", model)
		}
	}
}

// A trailing or leading slash is not a qualification.  "zcode/" must not be
// read as module "zcode" with an empty model, and "/GLM-5.3" must not be read
// as module "".
func TestResolveDoesNotTreatAHalfSlashAsQualification(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "zcode", models: []Model{{ID: "GLM-5.3"}}})

	for _, model := range []string{"zcode/", "/GLM-5.3"} {
		if _, _, err := r.Resolve(context.Background(), model); err == nil {
			t.Fatalf("Resolve(%q) accepted a half-qualified id", model)
		}
	}
}

// Whitespace around the whole request is trimmed, but the id itself is not
// rewritten — the module must receive exactly what its catalogue lists.
func TestResolveTrimsTheWholeRequestButNotTheID(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "cline", models: []Model{{ID: "cline-free/gemini-3.8-flash"}}})

	gotClient, gotModel := resolveOK(t, r, "  cline-free/gemini-3.8-flash  ")
	if gotClient != "cline" || gotModel != "cline-free/gemini-3.8-flash" {
		t.Fatalf("Resolve = (%q, %q), want (cline, cline-free/gemini-3.8-flash)", gotClient, gotModel)
	}
}

// A module whose catalogue is broken must not stop the search: Resolve skips
// the erroring module and keeps looking.
func TestResolveSkipsAModuleThatCannotListItsModels(t *testing.T) {
	broken := &brokenModelsClient{fakeClient{name: "broken"}}
	r := registryWith(t, broken, &fakeClient{name: "cline", models: []Model{{ID: "cline-free/gemini-3.8-flash"}}})

	gotClient, gotModel := resolveOK(t, r, "cline-free/gemini-3.8-flash")
	if gotClient != "cline" || gotModel != "cline-free/gemini-3.8-flash" {
		t.Fatalf("Resolve = (%q, %q), want (cline, cline-free/gemini-3.8-flash)", gotClient, gotModel)
	}
}

type brokenModelsClient struct{ fakeClient }

func (b *brokenModelsClient) Models(context.Context) ([]Model, error) {
	return nil, errFakeNoChat
}
