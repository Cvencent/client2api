package gateway

import (
	"io"
	"log"
	"net/http"
	"testing"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

func TestModelOverrideMaxTokensWins(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("mod", "m1", 0, 777); err != nil {
		t.Fatal(err)
	}
	base := &testClient{name: "mod", catalogue: []core.Model{{ID: "m1"}}}
	limits := &limitsClient{testClient: base, limits: map[string]int{"m1": 128000}}
	reg := core.NewRegistry()
	reg.Add(limits)
	srv := NewServer(Options{
		Registry:       reg,
		ModelOverrides: store,
		Logger:         log.New(io.Discard, "", 0),
	})

	if rec := chat(t, srv, noMaxTokensBody); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if base.seen == nil || base.seen.MaxTokens == nil {
		t.Fatal("max_tokens was not filled")
	}
	if got := *base.seen.MaxTokens; got != 777 {
		t.Fatalf("max_tokens = %d, want manual override 777", got)
	}
	if limits.calls != 0 {
		t.Fatalf("module limits were consulted %d times despite a manual override", limits.calls)
	}
}
