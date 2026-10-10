package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"client2api/internal/core"
	"client2api/internal/gateway"
)

type catalogueTestClient struct {
	name   string
	models []core.Model
}

func (c *catalogueTestClient) Name() string { return c.name }

func (c *catalogueTestClient) Models(context.Context) ([]core.Model, error) {
	return c.models, nil
}

func (c *catalogueTestClient) Chat(context.Context, *core.ChatRequest) (core.Stream, error) {
	return nil, nil
}

func (c *catalogueTestClient) Status(context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

func TestModelGroupsIncludeDefaultGPT6NativeNames(t *testing.T) {
	cfg := &fileConfig{}
	got := cfg.modelGroups()

	want := map[string][]string{
		"gpt-6-astra":     {"opencode/gpt-6-astra"},
		"gpt-6.1-sol":     {"opencode/gpt-6.1-sol"},
		"gpt-6-sol":       {"opencode/gpt-6-sol", "openrouter/openai/gpt-6-sol"},
		"gpt-6-luna":      {"opencode/gpt-6-luna", "openrouter/openai/gpt-6-luna"},
		"gpt-6.1-sol-pro": {"openrouter/openai/gpt-6.1-sol-pro"},
	}
	for name, wantMembers := range want {
		group, ok := got[name]
		if !ok {
			t.Fatalf("default model group %q is missing: %v", name, got)
		}
		gotMembers := make([]string, 0, len(group.Members))
		for _, member := range group.Members {
			gotMembers = append(gotMembers, member.Client+"/"+member.Model)
		}
		if !reflect.DeepEqual(gotMembers, wantMembers) {
			t.Fatalf("default model group %q members = %v, want %v", name, gotMembers, wantMembers)
		}
		if !group.Builtin {
			t.Fatalf("default model group %q is not marked built-in", name)
		}
	}
}

func TestConfiguredModelGroupOverridesDefault(t *testing.T) {
	cfg := &fileConfig{ModelGroups: map[string]modelGroupConfig{
		"GPT-6-SOL": {
			Members: []string{"custom/gpt-6-sol"},
		},
	}}

	group, ok := cfg.modelGroups()["gpt-6-sol"]
	if !ok {
		t.Fatal("configured gpt-6-sol group replaced the default with nothing")
	}
	if len(group.Members) != 1 || group.Members[0].Client != "custom" || group.Members[0].Model != "gpt-6-sol" {
		t.Fatalf("configured gpt-6-sol members = %+v, want the configured override", group.Members)
	}
}

func TestConfiguredEmptyModelGroupSuppressesDefaultCaseInsensitively(t *testing.T) {
	cfg := &fileConfig{ModelGroups: map[string]modelGroupConfig{
		"GPT-6-SOL": {},
	}}

	if _, ok := cfg.modelGroups()["gpt-6-sol"]; ok {
		t.Fatal("an explicitly empty default model group should stay disabled")
	}
}

func TestDefaultGPT6ModelGroupsAppearInModelsCatalogue(t *testing.T) {
	registry := core.NewRegistry()
	registry.Add(&catalogueTestClient{
		name: "opencode",
		models: []core.Model{
			{ID: "gpt-6-astra"},
			{ID: "gpt-6.1-sol"},
			{ID: "gpt-6-sol"},
			{ID: "gpt-6-luna"},
		},
	})
	registry.Add(&catalogueTestClient{
		name: "openrouter",
		models: []core.Model{
			{ID: "openai/gpt-6-sol"},
			{ID: "openai/gpt-6-luna"},
			{ID: "openai/gpt-6.1-sol-pro"},
		},
	})
	registry.SetModelGroups((&fileConfig{}).modelGroups())

	srv := gateway.NewServer(gateway.Options{
		Registry: registry,
		Logger:   log.New(io.Discard, "", 0),
	})
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /v1/models: %v (%s)", err, rec.Body.String())
	}
	seen := make(map[string]bool, len(out.Data))
	for _, entry := range out.Data {
		seen[entry.ID] = true
	}
	for _, want := range []string{
		"gpt-6-astra",
		"gpt-6.1-sol",
		"gpt-6-sol",
		"gpt-6-luna",
		"gpt-6.1-sol-pro",
	} {
		if !seen[want] {
			t.Errorf("/v1/models is missing native GPT-6 name %q: %s", want, rec.Body.String())
		}
	}
}
