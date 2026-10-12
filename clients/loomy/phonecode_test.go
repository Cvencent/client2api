package loomy

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPhoneCodeLoginSendsToTheOperatorsNumberAndStoresTheSession(t *testing.T) {
	vendor := newFakeVendor()
	srv := httptest.NewServer(vendor.handler())
	defer srv.Close()

	c := newTestClientInDir(t, t.TempDir(), autoLoginConfig(srv), srv.Client().Transport)
	const phone = "13900000001"

	challenge, err := c.StartPhoneCodeLogin(context.Background(), phone)
	if err != nil {
		t.Fatalf("StartPhoneCodeLogin: %v", err)
	}
	if challenge.ID == "" {
		t.Fatal("challenge id is empty")
	}
	if challenge.ExpiresAt == "" {
		t.Fatal("challenge expiry is empty")
	}

	vendor.mu.Lock()
	if !vendor.codeAsked {
		t.Fatal("the vendor was never asked to text a code")
	}
	if vendor.sendPhone != phone {
		t.Fatalf("sendMsgCode phone = %q, want %q", vendor.sendPhone, phone)
	}
	vendor.mu.Unlock()

	record, err := c.FinishPhoneCodeLogin(context.Background(), challenge.ID, vendor.code)
	if err != nil {
		t.Fatalf("FinishPhoneCodeLogin: %v", err)
	}
	if record.ID == "" || record.Label != phone {
		t.Fatalf("record = %+v, want a stored account labelled with the phone", record)
	}

	vendor.mu.Lock()
	if vendor.redeemed != vendor.code {
		t.Errorf("redeemed code = %q, want %q", vendor.redeemed, vendor.code)
	}
	if vendor.redeemedPhone != phone {
		t.Errorf("checkCode phone = %q, want %q", vendor.redeemedPhone, phone)
	}
	if vendor.redeemedMsgID != "m-1" {
		t.Errorf("checkCode msgid = %q, want m-1", vendor.redeemedMsgID)
	}
	vendor.mu.Unlock()

	accounts := c.store.snapshot()
	if len(accounts) != 1 {
		t.Fatalf("stored accounts = %d, want 1", len(accounts))
	}
	if accounts[0].Phone != phone || accounts[0].AccessToken != testToken {
		t.Fatalf("stored account = %+v, want the session for %s", accounts[0].storedAccount, phone)
	}

	if _, err := c.FinishPhoneCodeLogin(context.Background(), challenge.ID, vendor.code); err == nil {
		t.Fatal("a redeemed challenge was accepted a second time")
	}
}

func TestPhoneCodeStartRejectsAnObviousPhoneTypoWithoutCallingTheVendor(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	if _, err := c.StartPhoneCodeLogin(context.Background(), "12345"); err == nil {
		t.Fatal("StartPhoneCodeLogin accepted a malformed phone number")
	}
	if len(c.phoneChallenges) != 0 {
		t.Fatalf("a rejected phone created %d challenges", len(c.phoneChallenges))
	}
}

func TestPhoneCodeFinishRejectsUnknownAndExpiredChallenges(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	if _, err := c.FinishPhoneCodeLogin(context.Background(), "missing", "123456"); err == nil {
		t.Fatal("an unknown challenge was accepted")
	}

	vendor := newFakeVendor()
	srv := httptest.NewServer(vendor.handler())
	defer srv.Close()
	c = newTestClientInDir(t, t.TempDir(), autoLoginConfig(srv), srv.Client().Transport)

	challenge, err := c.StartPhoneCodeLogin(context.Background(), "13900000002")
	if err != nil {
		t.Fatalf("StartPhoneCodeLogin: %v", err)
	}
	c.phoneMu.Lock()
	c.phoneChallenges[challenge.ID].expiresAt = time.Now().UTC().Add(-time.Second)
	c.phoneMu.Unlock()

	if _, err := c.FinishPhoneCodeLogin(context.Background(), challenge.ID, vendor.code); err == nil {
		t.Fatal("an expired challenge was accepted")
	}
	c.phoneMu.Lock()
	_, stillThere := c.phoneChallenges[challenge.ID]
	c.phoneMu.Unlock()
	if stillThere {
		t.Fatal("an expired challenge was left in memory")
	}
}
