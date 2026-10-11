package opencode

import (
	"context"
	"net/http"
	"testing"

	"client2api/internal/core"
)

func testFreeOnlyOn() *bool {
	on := true
	return &on
}

func testFreeOnlyOff() *bool {
	off := false
	return &off
}
func TestFreeOnlyFiltersServedModels(t *testing.T) {
	c := newTestClient(t, Config{FreeOnly: testFreeOnlyOn()})
	models := c.servedModels([]core.Model{
		{ID: "space-bunny-free"},
		{ID: "gpt-5.1"},
	})
	if len(models) != 1 || models[0].ID != "space-bunny-free" {
		t.Fatalf("servedModels = %v, want only the free model", modelIDsOf(models))
	}
}

func TestFreeOnlyRejectsPaidChatBeforeUpstream(t *testing.T) {
	var calls int
	c := newFakeClient(t, Config{FreeOnly: testFreeOnlyOn()}, func(*http.Request) (*http.Response, error) {
		calls++
		return sseResponse(happySSE), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")

	_, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	wantErrIs(t, err, core.ErrUnsupported)
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}
