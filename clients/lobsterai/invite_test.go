package lobsterai

import (
	"net/url"
	"strings"
	"testing"
)

// TestInvitationCodeRidesOnTheLoginURL pins the only thing this module has to
// do for the invitation: LobsterAI submits it with the login callback, so the
// code must be present as the `invitationCode` query parameter.
func TestInvitationCodeRidesOnTheLoginURL(t *testing.T) {
	cfg := Config{InvitationCode: " AXReH0 "}.normalize()
	raw := cfg.loginURL("http://127.0.0.1:65341/auth/callback", "state-1")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("loginURL is not a URL: %v", err)
	}
	// The login route is a hash route, so the query lives in the fragment.
	q, err := url.ParseQuery(strings.SplitN(u.Fragment, "?", 2)[1])
	if err != nil {
		t.Fatalf("fragment query does not parse: %v", err)
	}
	if got := q.Get("invitationCode"); got != "AXReH0" {
		t.Fatalf("invitationCode = %q, want the trimmed code", got)
	}
	if got := q.Get("source"); got != "electron" {
		t.Fatalf("source = %q, want electron", got)
	}
	if !strings.HasSuffix(u.Path, "/portal#/login") && !strings.Contains(raw, "/portal#/login?") {
		t.Fatalf("login URL = %q, want the portal login route", raw)
	}
}

// TestAnEmptyInvitationCodeAddsNoParameter keeps the default login byte-for-byte
// the same as before the feature existed.
func TestAnEmptyInvitationCodeAddsNoParameter(t *testing.T) {
	cfg := Config{}.normalize()
	raw := cfg.loginURL("http://127.0.0.1:65341/auth/callback", "state-1")
	if strings.Contains(raw, "invitationCode") {
		t.Fatalf("login URL = %q, must not carry an empty invitationCode", raw)
	}
}
