package kimi

// Account identity: which Kimi user a credential belongs to.
//
// The panel lists credentials, and one Kimi account can appear as several of
// them -- the panel's own device-flow login, the CLI's credential file, and any
// executable binding the operator imported.  Before this the panel showed one
// account as three rows, so an operator could not tell whether disabling one
// row left the account usable or killed it.
//
// The account is named by the token the credential holds, so it is read out of
// the token's payload and never out of the credential's name.  An API key names
// nobody, and then the row stays its own account rather than being guessed at.
//
// This file deliberately does NOT live in identity_test.go: that one covers the
// *wire* identity (device id, x-msh-device-model, chat headers), which is a
// different notion of "identity" entirely.

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// kimiJWT builds a token shaped like the vendor's, with an arbitrary payload.
// The signature is fake: nothing here verifies one, and nothing should.
func kimiJWT(payload string) string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}

// ---------------------------------------------------------------------------
// Reading the account out of a credential blob
// ---------------------------------------------------------------------------

func TestCredentialIdentityReadsTheAccountOutOfTheToken(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "the user_id claim",
			body: `{"access_token":"` + kimiJWT(`{"user_id":"u-1"}`) + `"}`,
			want: "u-1",
		},
		{
			name: "the sub claim, when there is no user_id",
			body: `{"access_token":"` + kimiJWT(`{"sub":"u-2"}`) + `"}`,
			want: "u-2",
		},
		{
			name: "a numeric account id, which must not be rounded",
			body: `{"access_token":"` + kimiJWT(`{"user_id":61161790588087632}`) + `"}`,
			want: "61161790588087632",
		},
		{
			name: "a field the module does not know the token is under",
			body: `{"accessToken":"` + kimiJWT(`{"user_id":"u-3"}`) + `"}`,
			want: "u-3",
		},
		{
			name: "a token that names nobody stays unknown",
			body: `{"access_token":"` + kimiJWT(`{"scope":"kimi-code"}`) + `"}`,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := credentialIdentity([]byte(tc.body))
			if got != tc.want {
				t.Fatalf("credentialIdentity = %q, want %q", got, tc.want)
			}
			// The whole point of returning an identity rather than the token is
			// that the token never reaches the panel.  A JWT is three
			// dot-separated parts, so a value with dots in it is a leak.
			if strings.Contains(got, ".") {
				t.Errorf("the identity looks like a token: %q", got)
			}
		})
	}
}

func TestCredentialIdentityIsEmptyWhenTheCredentialNamesNoAccount(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "a bare API key", body: `{"access_token":"sk-kimi-abcdef"}`},
		{name: "an API key under another field", body: `{"api_key":"sk-kimi-abcdef"}`},
		{name: "a JWT that names no user", body: `{"access_token":"` + kimiJWT(`{"scope":"kimi-code"}`) + `"}`},
		{name: "no field the module reads", body: `{"something_else":"x"}`},
		{name: "an empty token", body: `{"access_token":""}`},
		{name: "a token that is not a string", body: `{"access_token":42}`},
		{name: "not JSON at all", body: `not json`},
		{name: "an empty blob", body: ``},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := credentialIdentity([]byte(tc.body)); got != "" {
				t.Fatalf("credentialIdentity = %q, want an unknown account", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// What a whole credential report agrees on
// ---------------------------------------------------------------------------

func TestTheCredentialReportAgreesOnOneAccountOnlyWhenTheCredentialsDo(t *testing.T) {
	src := func(found bool, identity string) credentialSource {
		return credentialSource{Kind: "file", Ref: "x.json", Found: found, Identity: identity}
	}

	tests := []struct {
		name    string
		sources []credentialSource
		want    string
	}{
		{
			name:    "two credentials for one account",
			sources: []credentialSource{src(true, "u-1"), src(true, "u-1")},
			want:    "u-1",
		},
		{
			name:    "two credentials naming different users say nothing",
			sources: []credentialSource{src(true, "u-1"), src(true, "u-2")},
			want:    "",
		},
		{
			name:    "one names the account and one does not",
			sources: []credentialSource{src(true, "u-1"), src(true, "")},
			want:    "u-1",
		},
		{
			name:    "a credential that was not found is not evidence",
			sources: []credentialSource{src(true, "u-1"), src(false, "u-9")},
			want:    "u-1",
		},
		{
			name:    "nothing names the account",
			sources: []credentialSource{src(true, ""), src(true, "")},
			want:    "",
		},
		{
			name:    "no credentials at all",
			sources: nil,
			want:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep := credentialReport{sources: tc.sources}
			if got := rep.identity(); got != tc.want {
				t.Fatalf("identity = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The rows the panel actually renders
// ---------------------------------------------------------------------------

// kimiAccountFixture plants both of this account's credentials: a device-flow
// grant in this module's own store, and a credential file the CLI wrote.
func kimiAccountFixture(t *testing.T, webUser, cliUser string) *Client {
	t.Helper()
	credPath := filepath.Join(t.TempDir(), "kimi-credentials.json")
	credsFileAt(t, credPath, kimiJWT(`{"user_id":"`+cliUser+`"}`), "2099-01-02T03:04:05Z")

	c, _, _ := clientWithStubCLI(t, map[string]any{"credential_files": []string{credPath}})
	if err := c.saveToken(storedToken{
		AccessToken: kimiJWT(`{"user_id":"` + webUser + `"}`),
		ExpiresAt:   time.Now().Add(2 * time.Hour).Unix(),
		TokenType:   "Bearer",
	}); err != nil {
		t.Fatalf("saveToken: %v", err)
	}
	return c
}

func TestTwoKimiLoginsForOneAccountShareAnIdentity(t *testing.T) {
	c := kimiAccountFixture(t, "u-42", "u-42")
	recs := accountsOrFatal(t, c)

	web := findAccount(t, recs, webLoginID)
	cli := findAccount(t, recs, cliLoginID)
	file := findAccount(t, recs, "file:kimi-credentials.json")

	// This is the operator's complaint: one Kimi user, three rows.  Every row
	// must name the same account, so the panel draws one account with three
	// channels instead of three accounts.
	for _, rec := range []struct {
		what string
		got  string
	}{{"the panel login", web.Identity}, {"the CLI login", cli.Identity}, {"the credential file", file.Identity}} {
		if rec.got != "u-42" {
			t.Errorf("%s names %q, want the account it belongs to", rec.what, rec.got)
		}
	}
}

func TestTwoKimiLoginsForDifferentAccountsStayApart(t *testing.T) {
	// Two credentials naming different users must not be merged: an operator
	// disabling one would then be disabling something that is not theirs.
	c := kimiAccountFixture(t, "u-1", "u-2")
	recs := accountsOrFatal(t, c)

	if got := findAccount(t, recs, webLoginID).Identity; got != "u-1" {
		t.Errorf("the panel login names %q, want u-1", got)
	}
	if got := findAccount(t, recs, "file:kimi-credentials.json").Identity; got != "u-2" {
		t.Errorf("the credential file names %q, want u-2", got)
	}
	// The CLI row is a view onto the CLI's credential, so it follows that one.
	if got := findAccount(t, recs, cliLoginID).Identity; got != "u-2" {
		t.Errorf("the CLI login names %q, want u-2", got)
	}
}

func TestKimiRowsWithoutAnIdentityStayTheirOwnAccount(t *testing.T) {
	// An API key names nobody.  The row must come back with an empty identity
	// rather than a guess, because the panel treats empty as "cannot say" and
	// keeps such a row as its own account.
	credPath := filepath.Join(t.TempDir(), "kimi-credentials.json")
	credsFileAt(t, credPath, "sk-kimi-plain-api-key", "2099-01-02T03:04:05Z")
	c, _, _ := clientWithStubCLI(t, map[string]any{"credential_files": []string{credPath}})
	storedPanelToken(t, c) // a non-JWT grant, so the panel row names nobody either

	recs := accountsOrFatal(t, c)
	for _, id := range []string{webLoginID, cliLoginID, "file:kimi-credentials.json"} {
		if got := findAccount(t, recs, id).Identity; got != "" {
			t.Errorf("%s names %q, want an unknown account", id, got)
		}
	}
	if _, err := c.Accounts(context.Background()); err != nil {
		t.Fatalf("Accounts: %v", err)
	}
}
