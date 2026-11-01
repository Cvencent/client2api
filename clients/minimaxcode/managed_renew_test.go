package minimaxcode

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestRefreshClientIDsUsesStoredClientForUserAccount(t *testing.T) {
	c, _ := newTestClient(t, loginConfig(), nil)
	got := c.refreshClientIDs(Account{
		ID:       "minimaxcode:user/cn/abcdefghijkl",
		ClientID: oauthClientIDDesktop,
	})
	if len(got) == 0 || got[0] != oauthClientIDDesktop {
		t.Fatalf("refresh client ids = %q, want %q first", got, oauthClientIDDesktop)
	}
	for _, id := range got {
		if id == "abcdefghijkl" {
			t.Fatalf("refresh client ids treated the user identity hash as a client id: %q", got)
		}
	}
}

// A row the panel signed in itself keeps its refresh token in this module's own
// store (managed_accounts.json) and has no desktop auth.json behind it.  The
// renewal path has to treat that store as a durable home: the access token
// lapses after about an hour and the vendor retires the refresh token the
// moment it is exchanged, so a row that cannot renew is a row that asks to be
// signed in again every hour.
func TestManagedAccountRefreshesItsLapsedToken(t *testing.T) {
	const id = "minimaxcode:prod/cn/mcode-public"

	c, ft := newTestClient(t, loginConfig(), func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/oauth2/token" {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL)
			return jsonResponse(http.StatusNotFound, `{}`), nil
		}
		return jsonResponse(http.StatusOK,
			`{"access_token":"mmoat_renewed","refresh_token":"mmort_renewed","expires_in":3600}`), nil
	})

	c.pool.mu.Lock()
	c.pool.ensureLocked()
	c.pool.putManagedLocked(&Account{
		ID:           id,
		Label:        "MiniMax558560",
		Token:        "mmoat_lapsed",
		Source:       sourceManaged,
		Origin:       managedFile,
		Enabled:      true,
		Managed:      true,
		State:        stateReady,
		RefreshToken: "mmort_live",
		ClientID:     oauthClientIDDesktop,
		ExpiresAt:    time.Now().Add(-time.Minute),
	})
	err := c.pool.saveManagedLocked()
	c.pool.mu.Unlock()
	if err != nil {
		t.Fatalf("seed managed store: %v", err)
	}

	got := c.ensureFresh(context.Background(), id)
	if got.Token != "mmoat_renewed" {
		t.Fatalf("ensureFresh handed back %q; a managed row must renew in place", got.Token)
	}
	if got.RefreshToken != "mmort_renewed" {
		t.Fatalf("refreshed row holds refresh token %q, want the rotated one", got.RefreshToken)
	}
	if n := ft.count(); n != 1 {
		t.Fatalf("token endpoint was called %d times, want 1", n)
	}

	// The vendor retires the old refresh token the moment it is spent, so the
	// rotated pair has to be on disk before this process ends.
	row := managedRow(t, c, id)
	if row.Token != "mmoat_renewed" || row.RefreshToken != "mmort_renewed" {
		t.Fatalf("managed store kept %q / %q, want the rotated pair", row.Token, row.RefreshToken)
	}

	for _, rec := range c.pool.recordsForPanel() {
		if rec.ID == id && (rec.State != stateReady || rec.Note != "") {
			t.Fatalf("refreshed row is reported as %q (%q), want ready", rec.State, rec.Note)
		}
	}
}

// The runtime state file is keyed by account id, and saveLocked deliberately
// leaves managed rows out of that file.  An id that used to be a discovered row
// still has a stale entry, though, and a desktop directory left behind makes the
// startup pass treat the id as signed out.  Neither verdict may be pasted onto
// the managed row that replaced it, or every restart parks a healthy account as
// login_required and the panel asks for a re-login again.
func TestRestartDoesNotParkAHealthyManagedRow(t *testing.T) {
	const id = "minimaxcode:prod/cn/mcode-public"
	dir := t.TempDir()
	authDir := t.TempDir()

	future := time.Now().Add(45 * time.Minute).UTC().Format(time.RFC3339)
	writeFile(t, filepath.Join(dir, managedFile),
		`{"version":1,"accounts":[{"id":"`+id+`","label":"MiniMax558560","token":"mmoat_live",`+
			`"refresh_token":"mmort_live","client_id":"mcode-public","enabled":true,`+
			`"expires_at":"`+future+`"}]}`)
	// Left over from the days this id was a discovered row.
	writeFile(t, filepath.Join(dir, persistedFile),
		`{"version":1,"accounts":[{"id":"`+id+`","state":"invalid","failures":4,`+
			`"last_error":"HTTP 400: invalid signature","last_used":1790790076465}]}`)
	// The desktop client signed out; its directory is still on disk.
	writeFile(t, filepath.Join(authDir, "prod", "cn", "mcode-public", "auth-state.json"),
		`{"state":"anonymous"}`)

	cfg, err := json.Marshal(map[string]any{
		"auto_discover":   true,
		"auth_dir":        authDir,
		"oauth_token_url": "https://accounts.test/oauth2/token",
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	built, err := New(core.Deps{
		DataDir: dir,
		Config:  json.RawMessage(cfg),
		HTTPClient: &http.Client{Transport: &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{}`), nil
		}}},
		Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := built.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", built)
	}

	recs := c.pool.recordsForPanel()
	if len(recs) != 1 {
		t.Fatalf("pool holds %d rows, want 1: %+v", len(recs), recs)
	}
	if got := recs[0].Fields["source"]; got != sourceManaged {
		t.Fatalf("row source = %v, want the managed store to own it", got)
	}
	if recs[0].State != stateReady || recs[0].Note != "" {
		t.Fatalf("restart reported the managed row as %q (%q), want ready", recs[0].State, recs[0].Note)
	}
}

// TestEnsureFreshSkipsAnInvalidAccount pins issue #68: once the vendor has
// killed the authorization (markNeedsLogin -> state=invalid), the refresh
// token is revoked and every oauth exchange is guaranteed to answer
// invalid_grant.  The cmd-layer gates (#48 renew sweep, #52 balance sweep)
// stop the scheduler from ASKING about the account; this gate stops the module
// from spending a doomed token exchange whenever anything (chat, tasks, the
// balance sweep, check-in status) reaches ensureFresh anyway.
func TestEnsureFreshSkipsAnInvalidAccount(t *testing.T) {
	const id = "minimaxcode:prod/cn/mcode-public"

	c, ft := newTestClient(t, loginConfig(), func(req *http.Request) (*http.Response, error) {
		t.Errorf("token endpoint was called for an invalid account: %s %s", req.Method, req.URL)
		return jsonResponse(http.StatusOK,
			`{"access_token":"mmoat_should_not_happen","refresh_token":"mmort_x","expires_in":3600}`), nil
	})

	c.pool.mu.Lock()
	c.pool.ensureLocked()
	c.pool.putManagedLocked(&Account{
		ID:           id,
		Label:        "MiniMax-dead",
		Token:        "mmoat_lapsed",
		Source:       sourceManaged,
		Origin:       managedFile,
		Enabled:      true,
		Managed:      true,
		State:        stateInvalid,
		RefreshToken: "mmort_revoked",
		ClientID:     oauthClientIDDesktop,
		ExpiresAt:    time.Now().Add(-time.Minute),
	})
	_ = c.pool.saveManagedLocked()
	c.pool.mu.Unlock()

	got := c.ensureFresh(context.Background(), id)
	if got.Token != "mmoat_lapsed" {
		t.Fatalf("ensureFresh renewed a dead credential: token %q, want the lapsed one untouched", got.Token)
	}
	if got.State != stateInvalid {
		t.Fatalf("state = %q, want invalid preserved (login_required still stands)", got.State)
	}
	if n := ft.count(); n != 0 {
		t.Fatalf("token endpoint was called %d times, want 0 -- the revoked token must not be spent", n)
	}
}
