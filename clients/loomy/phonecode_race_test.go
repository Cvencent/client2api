package loomy

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestPhoneCodeChallengeIsRedeemedAtMostOnce pins the one-shot guarantee of the
// challenge table.
//
// FinishPhoneCodeLogin reads the challenge, calls the vendor, and only then
// deletes the entry.  Two callers that arrive together therefore both observe
// the entry and both spend the same one-shot vendor code -- the vendor answers
// the first caller only, so the loser sees a confusing failure, or worse, a
// second account write.  The entry has to be claimed under the lock, not merely
// read under it.
func TestPhoneCodeChallengeIsRedeemedAtMostOnce(t *testing.T) {
	vendor := newFakeVendor()
	srv := httptest.NewServer(vendor.handler())
	defer srv.Close()

	c := newTestClientInDir(t, t.TempDir(), autoLoginConfig(srv), srv.Client().Transport)
	challenge, err := c.StartPhoneCodeLogin(context.Background(), "13900000003")
	if err != nil {
		t.Fatalf("StartPhoneCodeLogin: %v", err)
	}

	// Two redemptions of the same challenge, started together.
	const callers = 2
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]error, callers)
	)
	wg.Add(callers)
	for i := range results {
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := c.FinishPhoneCodeLogin(context.Background(), challenge.ID, vendor.code)
			results[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d of %d concurrent redemptions succeeded, want exactly 1 (errors: %v)", ok, callers, results)
	}
}

// TestPhoneCodeMistypedCodeCanBeRetried pins the operator-facing half of the
// one-shot guarantee.
//
// A redemption is claimed under the lock, so two callers can never spend the
// same vendor code; but a *rejected* code is the common case and costs nothing
// but a retyping. Deleting the challenge on failure would force a new text
// every time a digit slipped, so the entry survives a vendor rejection and only
// a success retires it.
func TestPhoneCodeMistypedCodeCanBeRetried(t *testing.T) {
	vendor := newFakeVendor()
	// The first redemption is refused, the second is answered normally.
	vendor.rejects = 1
	srv := httptest.NewServer(vendor.handler())
	defer srv.Close()

	c := newTestClientInDir(t, t.TempDir(), autoLoginConfig(srv), srv.Client().Transport)
	challenge, err := c.StartPhoneCodeLogin(context.Background(), "13900000004")
	if err != nil {
		t.Fatalf("StartPhoneCodeLogin: %v", err)
	}

	if _, err := c.FinishPhoneCodeLogin(context.Background(), challenge.ID, "000000"); err == nil {
		t.Fatal("a wrong code was accepted")
	}

	// The challenge has to still be redeemable with the right code.
	record, err := c.FinishPhoneCodeLogin(context.Background(), challenge.ID, vendor.code)
	if err != nil {
		t.Fatalf("the correct code was refused after a mistype: %v", err)
	}
	if record.ID == "" {
		t.Fatal("the retry stored no account")
	}

	// And a successful redemption still retires the challenge exactly once.
	if _, err := c.FinishPhoneCodeLogin(context.Background(), challenge.ID, vendor.code); err == nil {
		t.Fatal("a spent challenge was redeemed a second time")
	}
}
