package dsml

import (
	"strings"
	"testing"
)

// A block that closes but yields no call used to be dropped without a trace.
// That turns "the model tried to call a tool and the gateway could not read it"
// into a clean-looking stop, which is the failure this test pins: the text is
// handed back so the caller can show and log it.
func TestFeedSurfacesUnparsableClosedBlock(t *testing.T) {
	p := NewParser()
	text, calls := p.Feed("before <tool_calls>not an invoke</tool_calls> after")
	if len(calls) != 0 {
		t.Fatalf("calls = %+v, want none", calls)
	}
	if !strings.Contains(text, "not an invoke") {
		t.Fatalf("the unparsable block was swallowed: %q", text)
	}
	if !strings.Contains(text, "before") || !strings.Contains(text, "after") {
		t.Fatalf("visible text around the block was lost: %q", text)
	}
}

// A nameless invoke is the other silent-drop path: the element matches but
// carries no tool name, so nothing is callable.
func TestFeedSurfacesNamelessInvoke(t *testing.T) {
	p := NewParser()
	text, calls := p.Feed(`<invoke><parameter name="arguments">{}</parameter></invoke>`)
	if len(calls) != 0 {
		t.Fatalf("calls = %+v, want none", calls)
	}
	if !strings.Contains(text, "invoke") {
		t.Fatalf("the nameless invoke was swallowed: %q", text)
	}
}

// The well-formed path must keep working exactly as before.
func TestFeedStillParsesWellFormedBlock(t *testing.T) {
	p := NewParser()
	block := `<tool_calls><invoke name="exec"><parameter name="arguments" string="true">{"cmd":"ls"}</parameter></invoke></tool_calls>`
	text, calls := p.Feed("hi " + block)
	if len(calls) != 1 || calls[0].Name != "exec" {
		t.Fatalf("calls = %+v, want one exec", calls)
	}
	if strings.Contains(text, "tool_calls") || strings.Contains(text, "invoke") {
		t.Fatalf("a parsed block must not leak into the visible text: %q", text)
	}
	if text != "hi " {
		t.Fatalf("visible text = %q, want %q", text, "hi ")
	}
}
