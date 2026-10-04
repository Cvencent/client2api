package loomy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// A cancelled caller says nothing about the credential.
//
// The panel's "stop" button and a browser that navigates away both cancel the
// request context.  Before the guard in Chat, that cancellation came back from
// the transport looking exactly like an upstream failure, so one interrupted
// request parked a perfectly healthy account -- and, because max_attempts is 3,
// it burned the next two candidates as well.  The panel then reported every
// later request as "every account is parked or cooling down" for the whole
// cooldown window.
//
// The companion control is TestChatCoolsDownOnATransportFailure in
// client_test.go: a transport failure with a live caller still cools the
// account down, which is the behaviour this guard must not swallow.
func TestACancelledCallerDoesNotParkTheAccount(t *testing.T) {
	c := newTestClient(t, "{}", failingTransport{err: context.Canceled})
	seedAccount(t, c, "loomy-1", testToken, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the operator pressed Stop before the first byte came back

	if _, err := c.Chat(ctx, &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("Chat returned a stream for an already-cancelled context")
	}

	acc, ok := c.store.lookup("loomy-1")
	if !ok {
		t.Fatal("the account vanished from the store")
	}
	if acc.dead {
		t.Error("a cancelled caller parked the account as dead")
	}
	if !acc.cooldownTill.IsZero() {
		t.Errorf("a cancelled caller cooled the account down until %s", acc.cooldownTill)
	}
	if acc.failures != 0 {
		t.Errorf("a cancelled caller counted %d failures against the account", acc.failures)
	}
	if got := len(c.store.candidates(time.Now().UTC())); got != 1 {
		t.Errorf("candidates = %d, want 1: the credential must stay in the pool", got)
	}
}

// The same cancellation with several candidates must not burn the ones it never
// tried.  max_attempts is 3, so without the guard a single Stop could park the
// whole pool in one request.
func TestACancelledCallerDoesNotBurnTheOtherCandidates(t *testing.T) {
	c := newTestClient(t, "{}", failingTransport{err: context.Canceled})
	seedAccount(t, c, "loomy-1", testToken, 0)
	seedAccount(t, c, "loomy-2", testTokenTwo, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Chat(ctx, &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("Chat returned a stream for an already-cancelled context")
	}

	for _, id := range []string{"loomy-1", "loomy-2"} {
		acc, ok := c.store.lookup(id)
		if !ok {
			t.Fatalf("%s vanished from the store", id)
		}
		if acc.dead {
			t.Errorf("%s was parked as dead by a cancelled caller", id)
		}
		if !acc.cooldownTill.IsZero() {
			t.Errorf("%s was cooled down until %s by a cancelled caller", id, acc.cooldownTill)
		}
	}
	if got := len(c.store.candidates(time.Now().UTC())); got != 2 {
		t.Errorf("candidates = %d, want 2: one Stop must not empty the pool", got)
	}
}

// A deadline is the other half of the same question.  Loomy's own chat timeout
// arrives as context.DeadlineExceeded, and that one IS evidence: the vendor
// stopped answering, so the account should cool down.  Only a plain
// cancellation from the caller is exempt.
func TestOurOwnTimeoutStillCoolsTheAccountDown(t *testing.T) {
	// chat_timeout fires first and first_byte_timeout is out of the way, so the
	// failure reported here is the chat deadline rather than our no-response
	// watchdog.
	c := newTestClient(t, `{"chat_timeout":"20ms","first_byte_timeout":"30s"}`, deadlineTransport{})
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("Chat succeeded although the transport never answered")
	}

	acc, _ := c.store.lookup("loomy-1")
	if acc.dead {
		t.Error("a timeout parked the account as dead")
	}
	if acc.selectable(time.Now()) {
		t.Error("our own chat timeout did not cool the account down")
	}
}

// The panel redraws the accounts table every few seconds, and every redraw
// reads the balances it is showing.  A redraw cancels the reads still in
// flight.  A caller that walked away says nothing about the credential, so the
// read must leave the account exactly as it found it -- otherwise merely
// looking at the panel would park every account on screen.
func TestACancelledCallerDoesNotParkTheAccountOnABalanceRead(t *testing.T) {
	c := newTestClient(t, "{}", failingTransport{err: context.Canceled})
	seedAccount(t, c, "loomy-1", testToken, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.AccountBalance(ctx, "loomy-1", 0); err == nil {
		t.Fatal("AccountBalance succeeded although the caller had already gone")
	}
	assertNotParked(t, c, "loomy-1")
}

// Same read, different projection: the packages view is redrawn just as often.
func TestACancelledCallerDoesNotParkTheAccountOnAPackageRead(t *testing.T) {
	c := newTestClient(t, "{}", failingTransport{err: context.Canceled})
	seedAccount(t, c, "loomy-1", testToken, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.AccountPackages(ctx, "loomy-1"); err == nil {
		t.Fatal("AccountPackages succeeded although the caller had already gone")
	}
	assertNotParked(t, c, "loomy-1")
}

// The synchronous catalogue refresh is the panel's "re-fetch from upstream"
// button.  Closing the page while it runs must not cost the account.
func TestACancelledCallerDoesNotParkTheAccountOnACatalogueRefresh(t *testing.T) {
	c := newTestClient(t, "{}", failingTransport{err: context.Canceled})
	seedAccount(t, c, "loomy-1", testToken, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.RefreshModels(ctx); err == nil {
		t.Fatal("RefreshModels succeeded although the caller had already gone")
	}
	assertNotParked(t, c, "loomy-1")
}

// A refresh in flight when the operator navigates away must not park the
// account either: Loomy has no refresh endpoint, so a parking here costs a
// re-login.
func TestACancelledCallerDoesNotParkTheAccountOnARefresh(t *testing.T) {
	c := newTestClient(t, "{}", failingTransport{err: context.Canceled})
	seedAccount(t, c, "loomy-1", testToken, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results, err := c.RefreshAccount(ctx, "loomy-1")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	for _, r := range results {
		if r.OK {
			t.Error("a refresh whose caller had already gone was reported as OK")
		}
	}
	assertNotParked(t, c, "loomy-1")
}

// The task centre is read on every panel redraw too.
func TestACancelledCallerDoesNotParkTheAccountOnTheTaskBoard(t *testing.T) {
	c := newTestClient(t, "{}", failingTransport{err: context.Canceled})
	seedAccount(t, c, "loomy-1", testToken, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Tasks(ctx, "loomy-1"); err == nil {
		t.Fatal("Tasks succeeded although the caller had already gone")
	}
	assertNotParked(t, c, "loomy-1")
}

// The counterpart: a deadline *we* set is evidence about the credential -- the
// vendor did not answer in time -- so it must still cool the account down.
// Only context.Canceled means the caller left.
func TestOurOwnBalanceTimeoutStillCoolsTheAccountDown(t *testing.T) {
	c := newTestClient(t, `{"request_timeout":"20ms"}`, deadlineTransport{})
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.AccountBalance(context.Background(), "loomy-1", 0); err == nil {
		t.Fatal("AccountBalance succeeded although the transport never answered")
	}

	acc, _ := c.store.lookup("loomy-1")
	if acc.dead {
		t.Error("a timeout parked the account as dead")
	}
	if acc.selectable(time.Now()) {
		t.Error("our own request timeout did not cool the account down")
	}
}

// assertNotParked fails if a caller that walked away left any mark at all on
// the account: a cooldown, a failure count or a dead flag.
func assertNotParked(t *testing.T, c *Client, id string) {
	t.Helper()
	acc, ok := c.store.lookup(id)
	if !ok {
		t.Fatalf("%s vanished from the store", id)
	}
	if acc.dead {
		t.Errorf("%s was parked as dead by a caller that walked away", id)
	}
	if !acc.cooldownTill.IsZero() {
		t.Errorf("%s was cooled down until %s by a caller that walked away", id, acc.cooldownTill)
	}
	if acc.failures != 0 {
		t.Errorf("%s counted %d failures against a caller that walked away", id, acc.failures)
	}
	if !acc.selectable(time.Now()) {
		t.Errorf("%s is not selectable after a caller walked away", id)
	}
}

// deadlineTransport fails the way a real transport does when a deadline in the
// request context expires: it waits for the context and reports why it ended.
type deadlineTransport struct{}

func (deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	<-r.Context().Done()
	return nil, r.Context().Err()
}
