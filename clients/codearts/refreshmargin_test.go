package codearts

import (
	"context"
	"testing"
	"time"
)

func TestAccountsPublishRefreshMarginForAutomaticRenewal(t *testing.T) {
	c := newTestClient(t, `{"access_key_id":"AK","secret_access_key":"SK","security_token":"TOK","refresh_margin":"7m"}`)

	accounts, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	got, ok := accounts[0].Fields["refresh_margin_seconds"]
	if !ok {
		t.Fatal("refresh_margin_seconds is missing; the scheduler would have to guess")
	}
	if got != int64((7 * time.Minute).Seconds()) {
		t.Fatalf("refresh_margin_seconds = %v, want %d", got, int64((7 * time.Minute).Seconds()))
	}
}
