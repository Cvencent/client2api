package dsml

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The tool names Codex offers for this workload; the parser only treats a
// named element as a call when the name is one of these.
var namedTestTools = []string{"exec_command", "apply_patch", "write_stdin"}

// TestParserNamedToolElement covers the spelling that leaked on 2026-10-24:
// the call is the tool's own element with `parameter` children, not an
// `<invoke name="...">`.
func TestParserNamedToolElement(t *testing.T) {
	text := "<exec_command>\n" +
		"<parameter name=\"cmd\">git status</parameter>\n" +
		"<parameter name=\"workdir\">/repo</parameter>\n" +
		"</exec_command>"
	p := NewParserWithTools(namedTestTools)
	vis, calls := p.Feed(text)
	if vis != "" {
		t.Fatalf("visible = %q, want empty", vis)
	}
	if len(calls) != 1 || calls[0].Name != "exec_command" {
		t.Fatalf("calls = %+v", calls)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &got); err != nil {
		t.Fatalf("arguments %q: %v", calls[0].Arguments, err)
	}
	if got["cmd"] != "git status" || got["workdir"] != "/repo" {
		t.Fatalf("arguments = %#v", got)
	}
}

// A named element that is not one of the caller's tools must stay prose: the
// whole point of passing the tool list is to avoid swallowing ordinary markup.
func TestParserNamedToolElementIgnoresUnknownNames(t *testing.T) {
	text := "the <section> element wraps <parameter name=\"x\">1</parameter></section>"
	p := NewParserWithTools(namedTestTools)
	vis, calls := p.Feed(text)
	if len(calls) != 0 {
		t.Fatalf("calls = %+v, want none", calls)
	}
	if vis != text {
		t.Fatalf("visible = %q, want the original text", vis)
	}
}

// Without a tool list the named spelling is not guessed at.
func TestParserWithoutToolsLeavesNamedElementsAlone(t *testing.T) {
	text := "<exec_command><parameter name=\"cmd\">ls</parameter></exec_command>"
	p := NewParser()
	vis, calls := p.Feed(text)
	if len(calls) != 0 || vis != text {
		t.Fatalf("calls = %+v visible = %q", calls, vis)
	}
}

// A named call cut off before its close tag is held and recovered at Finish.
func TestParserTruncatedNamedToolElement(t *testing.T) {
	text := "<exec_command>\n<parameter name=\"cmd\">ls</parameter>\n"
	p := NewParserWithTools(namedTestTools)
	if vis, calls := p.Feed(text); vis != "" || len(calls) != 0 {
		t.Fatalf("mid-stream: visible=%q calls=%+v", vis, calls)
	}
	tail, calls := p.Finish()
	if tail != "" {
		t.Fatalf("finish leaked text: %q", tail)
	}
	if len(calls) != 1 || calls[0].Name != "exec_command" || calls[0].Arguments != "{\"cmd\":\"ls\"}" {
		t.Fatalf("calls = %+v", calls)
	}
}

// TestRealLeakedNamedToolElement replays the byte-exact assistant message that
// leaked into a Codex conversation on 2026-10-24, prose included.
func TestRealLeakedNamedToolElement(t *testing.T) {
	raw, err := os.ReadFile("testdata/named_tool_element.txt")
	if err != nil {
		t.Skipf("sample not present: %v", err)
	}
	p := NewParserWithTools(namedTestTools)
	vis, calls := p.Feed(string(raw))
	if len(calls) != 1 {
		t.Fatalf("calls = %+v (visible %q)", calls, vis)
	}
	if calls[0].Name != "exec_command" {
		t.Fatalf("name = %q", calls[0].Name)
	}
	if strings.Contains(vis, "<exec_command>") || strings.Contains(vis, "<parameter") {
		t.Fatalf("markup leaked into visible text: %q", vis)
	}
	if !strings.Contains(vis, "重要发现") {
		t.Fatalf("the prose before the call was dropped: %q", vis)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &got); err != nil {
		t.Fatalf("arguments %q: %v", calls[0].Arguments, err)
	}
	want := map[string]string{
		"login":               "true",
		"max_output_tokens":   "1000",
		"sandbox_permissions": "use_default",
		"shell":               "powershell",
		"tty":                 "false",
		"workdir":             "D:\\aipassport\\one2passport-publish",
		"yield_time_ms":       "30000",
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("argument %q = %q, want %q\nall: %#v", k, got[k], v, got)
		}
	}
	if !strings.Contains(got["cmd"], "manual_token.py") {
		t.Fatalf("cmd = %q", got["cmd"])
	}
}
