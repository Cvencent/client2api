package doubao

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"client2api/internal/core"
)

// TestAccountManagerLifecycle pins add/list/test/enable/remove.
func TestAccountManagerLifecycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"user_id":"42","nickname":"tester"}}`))
	}))
	defer srv.Close()
	c := newWebClient(t, srv.URL)

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		"cookie": "sessionid=abc; ttwid=xyz",
		"label":  "mine",
	}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec.ID == "" {
		t.Fatalf("AddAccount returned no id")
	}
	// The cookie must not ride along in the record.
	if _, leaked := rec.Fields["cookie"]; leaked {
		t.Fatalf("account record leaked the cookie field: %+v", rec.Fields)
	}

	accounts, _ := c.Accounts(context.Background())
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accounts))
	}

	res, err := c.TestAccount(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount not ok: %+v", res)
	}

	if err := c.SetAccountEnabled(context.Background(), rec.ID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	if got := c.store.snapshot()[0].Enabled; got {
		t.Fatalf("account still enabled after disable")
	}

	if err := c.RemoveAccount(context.Background(), rec.ID); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if accounts, _ := c.Accounts(context.Background()); len(accounts) != 0 {
		t.Fatalf("got %d accounts after remove, want 0", len(accounts))
	}
}

// TestAddAccountRequiresCookie pins the form's required field.
func TestAddAccountRequiresCookie(t *testing.T) {
	c := newTestClient(t)
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{}}); err == nil {
		t.Fatalf("AddAccount with no cookie should fail")
	}
}

// TestTestAccountRejectedCookieIsNotOK pins the auth classification: a rejected
// cookie reports OK=false without an error (it is a result, not a call failure)
// and parks the account invalid.
func TestTestAccountRejectedCookieIsNotOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"login required"}`))
	}))
	defer srv.Close()
	c := newWebClient(t, srv.URL)
	rec, _ := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"cookie": "sessionid=abc"}})
	res, err := c.TestAccount(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("TestAccount returned an error: %v", err)
	}
	if res.OK {
		t.Fatalf("a rejected cookie reported OK")
	}
	if got := c.store.snapshot()[0].State; got != stateInvalid {
		t.Fatalf("state = %q, want invalid", got)
	}
}

// TestAccountFieldsExposeCookie pins the add form.
func TestAccountFieldsExposeCookie(t *testing.T) {
	c := newTestClient(t)
	fields := c.AccountFields(context.Background())
	if len(fields) == 0 || fields[0].Key != "cookie" || !fields[0].Required {
		t.Fatalf("AccountFields = %+v, want a required cookie field first", fields)
	}
}
