package core

import (
	"errors"
	"testing"
)

// TestNoteAccountFailureForwardsToTheInstalledReporter pins the module-to-gateway
// channel the gateway uses to park one (account, model) after a 429 that the
// module absorbed by rotating to another account.
func TestNoteAccountFailureForwardsToTheInstalledReporter(t *testing.T) {
	req := &ChatRequest{}
	var gotAccount string
	var gotErr error
	req.SetAccountFailureReporter(func(accountID string, err error) {
		gotAccount, gotErr = accountID, err
	})
	want := errors.New("too many requests")
	req.NoteAccountFailure("acct-1", want)
	if gotAccount != "acct-1" {
		t.Fatalf("account = %q, want acct-1", gotAccount)
	}
	if !errors.Is(gotErr, want) {
		t.Fatalf("err = %v, want %v", gotErr, want)
	}
}

func TestNoteAccountFailureIsSafeForEveryCallerShape(t *testing.T) {
	req := &ChatRequest{}
	calls := 0
	req.SetAccountFailureReporter(func(string, error) { calls++ })
	req.NoteAccountFailure("", errors.New("x"))
	req.NoteAccountFailure("acct-1", nil)
	if calls != 0 {
		t.Fatalf("reporter calls = %d, want 0 for an empty account or a nil error", calls)
	}
	req.NoteAccountFailure("acct-1", errors.New("x"))
	if calls != 1 {
		t.Fatalf("reporter calls = %d, want 1", calls)
	}
	// A request without a reporter, and a nil request, must not panic.
	(&ChatRequest{}).NoteAccountFailure("acct-1", errors.New("x"))
	req.SetAccountFailureReporter(nil)
	req.NoteAccountFailure("acct-1", errors.New("x"))
	req = nil
	req.NoteAccountFailure("acct-1", errors.New("x"))
}
