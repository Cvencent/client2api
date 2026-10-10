package core

import (
	"context"
	"testing"
)

type sourceTestClient struct {
	name    string
	cfg     SourceConfig
	updates int
}

func (c *sourceTestClient) Name() string                            { return c.name }
func (c *sourceTestClient) Models(context.Context) ([]Model, error) { return nil, nil }
func (c *sourceTestClient) Chat(context.Context, *ChatRequest) (Stream, error) {
	return nil, ErrUnsupported
}
func (c *sourceTestClient) Status(context.Context) Status { return Status{Name: c.name} }
func (c *sourceTestClient) ConfigureSource(cfg SourceConfig) error {
	c.cfg = cfg
	c.updates++
	return nil
}

func TestSourceValidation(t *testing.T) {
	for _, id := range []string{"../x", "foo/bar", "CON", "con", "aux", "com1", "OpenAI", "foo_", "", "auto"} {
		if err := ValidateSources(map[string]SourceConfig{id: {BaseURL: "https://relay.example/v1"}}); err == nil {
			t.Errorf("accepted id %q", id)
		}
	}
	for _, root := range []string{"file:///tmp/x", "https://key@relay.example/v1", "https://relay.example/v1?key=x", "http://", "https://relay.example/#x"} {
		if err := ValidateSources(map[string]SourceConfig{"relay": {BaseURL: root}}); err == nil {
			t.Errorf("accepted URL %q", root)
		}
	}
	if err := ValidateSources(map[string]SourceConfig{"my-relay": {Label: "My relay", BaseURL: "http://127.0.0.1:20128/v1"}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range Registered() {
		if err := ValidateSources(map[string]SourceConfig{name: {BaseURL: "https://relay.example/v1"}}); err == nil {
			t.Fatalf("accepted compiled module %s", name)
		}
	}
}

func TestSourceReconciliationRetainsInstancesAndRemovesRoutes(t *testing.T) {
	// A test-specific factory avoids changing the module factory used by other tests.
	r := NewRegistry()
	build := func(name string, cfg SourceConfig, deps Deps) (SourceClient, error) {
		return &sourceTestClient{name: name, cfg: cfg}, nil
	}
	cfg := map[string]SourceConfig{"my-relay": {BaseURL: "https://relay.example/v1"}}
	if err := r.reconcileSources(cfg, Deps{DataDir: t.TempDir()}, build); err != nil {
		t.Fatal(err)
	}
	first, _ := r.Get("my-relay")
	cfg["my-relay"] = SourceConfig{Label: "Edited", BaseURL: "https://relay.example/v2"}
	if err := r.reconcileSources(cfg, Deps{DataDir: t.TempDir()}, build); err != nil {
		t.Fatal(err)
	}
	updated, _ := r.Get("my-relay")
	if first != updated || updated.(*sourceTestClient).cfg.Label != "Edited" {
		t.Fatal("source instance lost on update")
	}
	if err := r.reconcileSources(nil, Deps{}, build); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("my-relay"); ok || len(r.Names()) != 0 {
		t.Fatal("removed source is still routed/listed")
	}
}
