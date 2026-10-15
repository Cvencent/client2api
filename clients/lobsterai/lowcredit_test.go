package lobsterai

import (
	"context"
	"io"
	"net/http"
	"testing"

	"client2api/internal/core"
)

// addAccountWithUID stores one account with an explicit uid, so a test can hold
// several of them.
func addAccountWithUID(t *testing.T, c *Client, token, uid string) core.AccountRecord {
	t.Helper()
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		"access_token": token,
		"uid":          uid,
	}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	return rec
}

// A hard-credit verdict must park the account out of rotation, not just write a
// note: a zero-credit account keeps answering HTTP 200 with "免费额度已用完"
// inside the stream, so a pool that keeps selecting it repeats the failure on
// every request instead of trying the platform's other credentials.
func TestHardCreditParksTheAccountOutOfRotation(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	rec := addAccount(t, f.client, "access-1")

	f.client.noteFailure(f.client.pool.byID(rec.ID), kindHardCredit, "免费额度已用完，请升级套餐")
	f.client.persist()

	now := f.client.now()
	if got := f.client.pool.ready(now, f.client.cfg.maxInFlight()); got != 0 {
		t.Fatalf("ready accounts = %d, want 0: the zero-credit account is still selectable", got)
	}
	if got := stateOf(f.client.pool.byID(rec.ID), now); got != "exhausted" {
		t.Fatalf("account state = %q, want exhausted", got)
	}
}

// The park is a verdict, not a credential problem: a balance read that proves
// credit is back must put the account into rotation again.
func TestFundedBalanceClearsTheHardCreditPark(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/user/profile-summary" {
			io.WriteString(w, `{"code":0,"data":{"totalCreditsRemaining":500}}`)
			return
		}
		http.NotFound(w, r)
	})
	rec := addAccount(t, f.client, "access-1")
	f.client.noteFailure(f.client.pool.byID(rec.ID), kindHardCredit, "免费额度已用完")
	f.client.persist()

	bal, err := f.client.AccountBalance(context.Background(), rec.ID, 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 500 {
		t.Fatalf("credits = %d, want 500", bal.Credits)
	}
	if got := f.client.pool.ready(f.client.now(), f.client.cfg.maxInFlight()); got != 1 {
		t.Fatalf("ready accounts = %d, want 1: a funded balance read did not clear the park", got)
	}
}

// The reported symptom: two zero-credit accounts were tried over and over while
// other platforms could have served the request.  Once both are parked the
// module must say so instead of burning another round on the same verdict.
func TestZeroCreditPoolReportsBusyRatherThanRetrying(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `data: {"error":{"message":"免费额度已用完，请升级套餐"}}`+"\n\n")
	})
	// Distinct uids: the id is derived from the uid, so two accounts sharing
	// one would collapse into a single record.
	first := addAccountWithUID(t, f.client, "access-1", "uid-1")
	second := addAccountWithUID(t, f.client, "access-2", "uid-2")

	// Two chat attempts, one per account.  The stream must be drained: the
	// refusal lives inside the SSE body, so it is only produced on read.
	for i := 0; i < 2; i++ {
		stream, err := f.client.Chat(context.Background(), &core.ChatRequest{
			Model:    "deepseek-flash",
			Messages: []core.Message{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			continue
		}
		for {
			ev, err := stream.Recv()
			if err != nil || ev.Type == core.EventError || ev.Type == core.EventDone {
				break
			}
		}
		stream.Close()
	}

	now := f.client.now()
	for _, id := range []string{first.ID, second.ID} {
		if got := stateOf(f.client.pool.byID(id), now); got != "exhausted" {
			t.Fatalf("account %s state = %q, want exhausted", id, got)
		}
	}
	if got := f.client.pool.ready(now, f.client.cfg.maxInFlight()); got != 0 {
		t.Fatalf("ready accounts = %d, want 0", got)
	}
}
