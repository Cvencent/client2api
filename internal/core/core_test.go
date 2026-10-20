package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The gateway allocates ServedBy on every request it serves, and a module calls
// NoteServedBy at the point it picks a credential.  The helper has to be safe
// for every caller shape, because a module cannot tell which one it has: a
// direct caller (a test, an embedder) may hand over a request with no slot at
// all, and a module that knows nothing may pass an empty id.
func TestNoteServedByIsSafeForEveryCallerShape(t *testing.T) {
	t.Run("nil request", func(t *testing.T) {
		NoteServedBy(nil, "acct-1") // must not panic
	})

	t.Run("no slot allocated", func(t *testing.T) {
		NoteServedBy(&ChatRequest{Model: "m1"}, "acct-1") // must not panic
	})

	t.Run("empty account id is not an answer", func(t *testing.T) {
		var served string
		req := &ChatRequest{ServedBy: &served}
		NoteServedBy(req, "")
		if served != "" {
			t.Errorf("ServedBy = %q, want it left empty", served)
		}
	})
}

func TestNoteServedByWritesTheAccount(t *testing.T) {
	var served string
	req := &ChatRequest{ServedBy: &served}

	NoteServedBy(req, "acct-1")
	if served != "acct-1" {
		t.Fatalf("ServedBy = %q, want acct-1", served)
	}

	// A rotating module names each attempt, and the gateway reads the slot once
	// after Chat returns.  The last writer must therefore win: the account that
	// actually got through is the one worth recording.
	NoteServedBy(req, "acct-2")
	if served != "acct-2" {
		t.Errorf("ServedBy = %q after a retry, want acct-2", served)
	}
}

// The per-account in-flight gate is installed by the gateway and used by every
// module.  A module cannot tell whether it is running under the gateway (a unit
// test or an embedder hands it a bare request), so the helper must be a no-op
// for every caller shape that has no hook.
func TestAccountSlotIsSafeWithoutAGateway(t *testing.T) {
	var nilReq *ChatRequest
	if err := nilReq.AcquireAccountSlot("acct-1"); err != nil {
		t.Errorf("nil request: AcquireAccountSlot = %v, want nil", err)
	}
	nilReq.ReleaseAccountSlot() // must not panic

	req := &ChatRequest{Model: "m1"}
	if err := req.AcquireAccountSlot(""); err != nil {
		t.Errorf("empty account: AcquireAccountSlot = %v, want nil", err)
	}
	if err := req.AcquireAccountSlot("acct-1"); err != nil {
		t.Errorf("no acquirer installed: AcquireAccountSlot = %v, want nil", err)
	}
	req.ReleaseAccountSlot() // must not panic with no lease
}

// Acquiring a different account is how a rotating module moves its lease: the
// old slot has to be handed back before the new one is kept, and a final
// ReleaseAccountSlot must return whatever is still held.
func TestAccountSlotSwitchesAndReleases(t *testing.T) {
	var released []string
	req := &ChatRequest{}
	req.SetAccountAcquirer(func(id string) (func(), error) {
		return func() { released = append(released, id) }, nil
	})

	if err := req.AcquireAccountSlot("acct-1"); err != nil {
		t.Fatalf("acquire acct-1: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("released %v before any switch, want nothing", released)
	}

	if err := req.AcquireAccountSlot("acct-2"); err != nil {
		t.Fatalf("acquire acct-2: %v", err)
	}
	if len(released) != 1 || released[0] != "acct-1" {
		t.Fatalf("released = %v after switching to acct-2, want [acct-1]", released)
	}

	req.ReleaseAccountSlot()
	if len(released) != 2 || released[1] != "acct-2" {
		t.Fatalf("released = %v after ReleaseAccountSlot, want [acct-1 acct-2]", released)
	}
	// Releasing twice must not double-release a slot.
	req.ReleaseAccountSlot()
	if len(released) != 2 {
		t.Fatalf("released = %v after a second ReleaseAccountSlot, want no extra release", released)
	}
}

// A refused acquisition is backpressure: the module should try another
// account, and the lease it already holds must survive the refusal.
func TestAccountSlotRefusalKeepsTheHeldLease(t *testing.T) {
	var released []string
	req := &ChatRequest{}
	req.SetAccountAcquirer(func(id string) (func(), error) {
		if id == "busy" {
			return nil, ErrBusy
		}
		return func() { released = append(released, id) }, nil
	})

	if err := req.AcquireAccountSlot("acct-1"); err != nil {
		t.Fatalf("acquire acct-1: %v", err)
	}
	if err := req.AcquireAccountSlot("busy"); !errors.Is(err, ErrBusy) {
		t.Fatalf("acquire busy = %v, want ErrBusy", err)
	}
	if len(released) != 0 {
		t.Fatalf("released = %v after a refused switch, want the held lease kept", released)
	}

	req.ReleaseAccountSlot()
	if len(released) != 1 || released[0] != "acct-1" {
		t.Fatalf("released = %v, want [acct-1]", released)
	}
}

// TestRepairToolArgumentsMakesEveryCallVendorSafe pins the invariant the
// gateway now enforces before any module sees a request: every replayed
// assistant tool call carries function.arguments that is a JSON object.
//
// The dirty inputs are real.  They were extracted from Codex session rollouts,
// where a model's tool-call output was stored with a missing quote, a leaked
// control token, or a truncated diff.  A strict upstream answers such a history
// with "Assistant tool call function.arguments must be valid JSON" and refuses
// the whole request, so the repair has to happen here rather than at the vendor.
func TestRepairToolArgumentsMakesEveryCallVendorSafe(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"already an object", `{"cmd":"ls"}`},
		{"empty", ""},
		{"whitespace", "   \n  "},
		{"literal null", "null"},
		{"missing quote around a bare word", `{"cmd": "node --check a.js", "prefix_rule": pwsh, "tty": false}`},
		{"leaked control token", `{"cmd": "x", "pwsh.exe</| : false, "workdir": "D:\\a"}`},
		{"unterminated string", `{"diff": "diff --git a/x b/x`},
		{"session id without quotes", `{"chars": "", "session_id": da7095}`},
		{"trailing bytes after the object", `{"cmd": "echo hi"}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []Message{{
				Role:      "assistant",
				ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Name: "exec_command", Arguments: tc.in}},
			}}
			got := RepairToolArguments(msgs)
			if len(got) != 1 || len(got[0].ToolCalls) != 1 {
				t.Fatalf("the call disappeared: %+v", got)
			}
			args := got[0].ToolCalls[0].Arguments
			if !json.Valid([]byte(args)) {
				t.Fatalf("arguments = %q, want valid JSON", args)
			}
			if !strings.HasPrefix(strings.TrimSpace(args), "{") {
				t.Fatalf("arguments = %q, want a JSON object", args)
			}
			// A repair must never silently lose the original payload: the tool
			// result in the history was keyed to this call, so dropping the text
			// would leave the replayed turn incoherent.
			// Already-valid JSON is returned byte for byte; empty/null payloads
			// become {} and have no original text worth retaining.
			if tc.name != "already an object" && strings.TrimSpace(tc.in) != "" && tc.in != "null" && !strings.Contains(args, "_raw") {
				t.Fatalf("arguments = %q, want the original text preserved under _raw", args)
			}
		})
	}

	t.Run("a well formed call is returned unchanged", func(t *testing.T) {
		msgs := []Message{{
			Role:      "assistant",
			ToolCalls: []ToolCall{{ID: "call_1", Name: "exec_command", Arguments: `{"cmd":"ls"}`}},
		}}
		got := RepairToolArguments(msgs)
		if got[0].ToolCalls[0].Arguments != `{"cmd":"ls"}` {
			t.Errorf("a valid call was rewritten: %q", got[0].ToolCalls[0].Arguments)
		}
	})

	t.Run("non-tool messages are untouched", func(t *testing.T) {
		msgs := []Message{
			{Role: "user", Content: `{"not":"a tool call"}`},
			{Role: "tool", ToolCallID: "call_1", Content: "plain text"},
		}
		got := RepairToolArguments(msgs)
		if got[0].Content != `{"not":"a tool call"}` || got[1].Content != "plain text" {
			t.Fatalf("non-tool messages were rewritten: %+v", got)
		}
	})
}
