package kimi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// A request that fails because the panel login was parked must say so.  The CLI
// path can only report what it knows, and before this it answered "run `kimi
// login`" to an operator who had signed in from the panel and whose account had
// been parked after the vendor refused the grant -- an instruction that repairs
// nothing, on a machine where the thing that needs fixing is the panel login.
func TestChatNamesTheParkedPanelLoginInsteadOfBlamingTheCLI(t *testing.T) {
	c, _, _ := clientWithStubCLI(t, nil)
	storedPanelToken(t, c)
	c.noteFailure(webLoginID, &upstreamStatusError{
		Where:  "https://api.kimi.com/coding/v1/chat/completions",
		Status: http.StatusForbidden,
	}, "", 0)
	if c.selectable(webLoginID) {
		t.Fatal("the fixture did not park the panel login, so this test proves nothing")
	}

	_, err := c.Chat(context.Background(), userReq("kimi", "hi"))
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("Chat error = %v, want core.ErrNotConfigured", err)
	}
	msg := err.Error()
	for _, want := range []string{webLoginID, "not usable right now", "403", "sign in again from the panel"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Chat error = %q, want it to contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "run `kimi login`") {
		t.Errorf("Chat error = %q, want it not to send the operator to the CLI login", msg)
	}
}

// An account the operator turned off is a decision, and the error has to point
// at the switch that undoes it rather than at a sign-in.
func TestChatPointsAtADisabledPanelLogin(t *testing.T) {
	c, _, _ := clientWithStubCLI(t, nil)
	storedPanelToken(t, c)
	if err := c.SetAccountEnabled(context.Background(), webLoginID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}

	_, err := c.Chat(context.Background(), userReq("kimi", "hi"))
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("Chat error = %v, want core.ErrNotConfigured", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "disabled in the panel") {
		t.Errorf("Chat error = %q, want it to name the disable", msg)
	}
	if !strings.Contains(msg, "re-enable it in the panel") {
		t.Errorf("Chat error = %q, want it to name the way back", msg)
	}
}

// With no panel login at all the CLI really is the only route, so the old
// instruction stands and no panel account is invented.
func TestChatStillBlamesTheCLIWhenThePanelNeverSignedIn(t *testing.T) {
	c, _, _ := clientWithStubCLI(t, nil)

	_, err := c.Chat(context.Background(), userReq("kimi", "hi"))
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("Chat error = %v, want core.ErrNotConfigured", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "run `kimi login`") {
		t.Errorf("Chat error = %q, want the CLI instruction when there is no panel login", msg)
	}
	if strings.Contains(msg, webLoginID) {
		t.Errorf("Chat error = %q, want no mention of a panel login that was never made", msg)
	}
}
