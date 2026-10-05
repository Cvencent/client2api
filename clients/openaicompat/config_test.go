package openaicompat

import (
	"encoding/json"
	"testing"
)

func TestParseConfigMultiProvider(t *testing.T) {
	raw := json.RawMessage(`{
		"providers": [
			{"id": "groq", "base_url": "https://api.groq.com/openai/v1", "api_key": "gsk_test", "models": ["llama-3.3-70b-versatile"]},
			{"id": "cerebras", "base_url": "https://api.cerebras.ai/v1", "api_key": "csk_test", "models": ["zai-glm-4.7"]},
			{"id": "siliconflow", "base_url": "https://api.siliconflow.com/v1", "api_key": "sk_sf", "models": ["deepseek-ai/DeepSeek-V3"]}
		]
	}`)
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.Providers) != 3 {
		t.Fatalf("expected 3 providers, got %d", len(cfg.Providers))
	}
	byID := map[string]ProviderConfig{}
	for _, p := range cfg.Providers {
		byID[p.ID] = p
	}
	for id, wantURL := range map[string]string{
		"groq":        "https://api.groq.com/openai/v1",
		"cerebras":    "https://api.cerebras.ai/v1",
		"siliconflow": "https://api.siliconflow.com/v1",
	} {
		p, ok := byID[id]
		if !ok {
			t.Fatalf("provider %q missing", id)
		}
		if p.BaseURL != wantURL {
			t.Errorf("provider %q base_url = %q, want %q", id, p.BaseURL, wantURL)
		}
		if p.APIKey == "" {
			t.Errorf("provider %q api_key empty", id)
		}
	}
}

func TestParseConfigEmpty(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig(nil): %v", err)
	}
	if len(cfg.Providers) != 0 {
		t.Fatalf("expected 0 providers, got %d", len(cfg.Providers))
	}
}

func TestParseConfigTrimsAndDedupes(t *testing.T) {
	raw := json.RawMessage(`{
		"providers": [
			{"id": "groq", "base_url": "https://api.groq.com/openai/v1/", "api_key": "k1"},
			{"id": "groq", "base_url": "https://api.groq.com/openai/v1", "api_key": "k1"},
			{"id": "cerebras", "base_url": "  https://api.cerebras.ai/v1  ", "api_key": "  k2  "}
		]
	}`)
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("expected 2 providers after dedupe, got %d", len(cfg.Providers))
	}
	if cfg.Providers[0].BaseURL != "https://api.groq.com/openai/v1" {
		t.Errorf("base_url not trimmed: %q", cfg.Providers[0].BaseURL)
	}
	if cfg.Providers[1].APIKey != "k2" {
		t.Errorf("api_key not trimmed: %q", cfg.Providers[1].APIKey)
	}
}

func TestParseConfigSkipsEmptyKey(t *testing.T) {
	raw := json.RawMessage(`{
		"providers": [
			{"id": "groq", "base_url": "https://api.groq.com/openai/v1", "api_key": ""},
			{"id": "cerebras", "base_url": "https://api.cerebras.ai/v1", "api_key": "k"}
		]
	}`)
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("expected 1 provider (empty key skipped), got %d", len(cfg.Providers))
	}
	if cfg.Providers[0].ID != "cerebras" {
		t.Errorf("kept wrong provider: %q", cfg.Providers[0].ID)
	}
}

func TestProviderDefaults(t *testing.T) {
	p := ProviderConfig{ID: "groq"}
	if got := p.chatURL(); got != "https://api.groq.com/openai/v1/chat/completions" {
		t.Errorf("groq chatURL = %q", got)
	}
	p = ProviderConfig{ID: "cerebras"}
	if got := p.chatURL(); got != "https://api.cerebras.ai/v1/chat/completions" {
		t.Errorf("cerebras chatURL = %q", got)
	}
	p = ProviderConfig{ID: "siliconflow"}
	if got := p.chatURL(); got != "https://api.siliconflow.com/v1/chat/completions" {
		t.Errorf("siliconflow chatURL = %q", got)
	}
}

func TestParseConfigDurations(t *testing.T) {
	raw := json.RawMessage(`{
		"providers": [{"id": "groq", "api_key": "k"}],
		"chat_timeout": "5m",
		"stream_idle_timeout": "60s"
	}`)
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.chatTimeout() != 5*60*1000*1000*1000 {
		t.Errorf("chatTimeout = %v", cfg.chatTimeout())
	}
	if cfg.streamIdle() != 60*1000*1000*1000 {
		t.Errorf("streamIdle = %v", cfg.streamIdle())
	}
}
