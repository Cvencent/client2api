package modelmeta

import "testing"

func TestFromExtraAutoKeys(t *testing.T) {
	m := FromExtra(map[string]any{
		"context_window":        200000,
		"max_completion_tokens": 131072,
	}, KeysCanonical, "")
	if m.ContextLength != 200000 {
		t.Fatalf("context = %d, want context_window fallback 200000", m.ContextLength)
	}
	if m.MaxOutputTokens != 131072 {
		t.Fatalf("max_output = %d, want max_completion_tokens fallback 131072", m.MaxOutputTokens)
	}
	if m.SourceOf(FieldContextLength) != SourceVendor || m.SourceOf(FieldMaxOutputTokens) != SourceVendor {
		t.Fatalf("sources = %+v", m.FieldSources)
	}

	m = FromExtra(map[string]any{"max_input_tokens": 300000}, KeysCanonical, "")
	if m.ContextLength != 300000 {
		t.Fatalf("context = %d, want max_input_tokens fallback 300000", m.ContextLength)
	}

	m = FromExtra(map[string]any{"context_length": 111, "context_window": 222}, KeysCanonical, "")
	if m.ContextLength != 111 {
		t.Fatalf("configured key should win, got %d", m.ContextLength)
	}
}
