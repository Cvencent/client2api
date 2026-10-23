package dsml

import (
	"encoding/json"
	"os"
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
	if !p.Malformed() {
		t.Fatal("the unparsable block was not reported as malformed")
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
	if !p.Malformed() {
		t.Fatal("the nameless invoke was not reported as malformed")
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

// Malformed distinguishes complete-but-unusable and truncated tool markup from
// ordinary visible text. The gateway can then fail the turn instead of leaking
// protocol syntax to the caller.
func TestMalformedReportsIncompleteBlock(t *testing.T) {
	p := NewParser()
	text, calls := p.Feed(`<tool_calls><invoke name="exec"><parameter name="arguments">{}`)
	if text != "" || len(calls) != 0 {
		t.Fatalf("text = %q calls = %+v, want both empty", text, calls)
	}
	text, calls = p.Finish()
	if text == "" || len(calls) != 0 {
		t.Fatalf("finish = %q, %+v; want leftover text and no calls", text, calls)
	}
	if !p.Malformed() {
		t.Fatal("truncated tool markup was not reported as malformed")
	}
}

// Upstreams do not always label the block.  Loomy's DeepSeek route emits the
// fullwidth-bar envelope with the "DSML" label already stripped: the tag is
// literally `<｜｜ calls>`, with nothing between the bars.  The name prefix is
// therefore optional in the upstream text even though the dialect is the same.
// A close tag may also carry a different number of bars than its opening tag
// ("</｜｜｜ parameter>" for "<｜｜ parameter>"), so closing tags are matched
// loosely rather than as mirror images.
func TestFeedParsesFullwidthBarMarkerlessBlock(t *testing.T) {
	text := "<｜｜ calls>\n" +
		"<｜｜ invoke name=\"exec\">\n" +
		"<｜｜ parameter name=\"cmd\" string=\"true\">git diff</｜｜｜ parameter>\n" +
		"</｜｜｜ invoke>\n" +
		"</｜｜｜ calls>"
	p := NewParser()
	vis, calls := p.Feed(text)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v (visible %q), want one call", calls, vis)
	}
	if calls[0].Name != "exec" || calls[0].Arguments != `{"cmd":"git diff"}` {
		t.Fatalf("call = %+v", calls[0])
	}
	if vis != "" {
		t.Fatalf("visible = %q, want empty", vis)
	}
	if p.Malformed() {
		t.Fatal("a well-formed markerless block was reported as malformed")
	}
}

// The same text arrives split across deltas, often one rune at a time.
func TestFeedParsesFullwidthBarMarkerlessBlockStreamed(t *testing.T) {
	full := "<｜｜ calls>\n" +
		"<｜｜ invoke name=\"exec\">\n" +
		"<｜｜ parameter name=\"cmd\" string=\"true\">ls</｜｜｜ parameter>\n" +
		"</｜｜｜ invoke>\n" +
		"</｜｜｜ calls>"
	p := NewParser()
	var calls []Call
	for _, r := range full {
		_, got := p.Feed(string(r))
		calls = append(calls, got...)
	}
	if text, got := p.Finish(); text != "" || len(got) != 0 {
		t.Fatalf("finish = %q %+v, want empty", text, got)
	}
	if len(calls) != 1 || calls[0].Name != "exec" || calls[0].Arguments != `{"cmd":"ls"}` {
		t.Fatalf("calls = %+v", calls)
	}
}

// The leaked envelopes name every parameter explicitly instead of packing the
// whole call into one `arguments` element, and all of them must survive.
func TestFeedMarkerlessBlockKeepsParameterNames(t *testing.T) {
	text := "<｜｜ tools>\n" +
		"<｜｜ invoke name=\"exec_command\">\n" +
		"<｜｜ parameter name=\"cmd\" string=\"true\">git status</｜｜｜ parameter>\n" +
		"<｜｜ parameter name=\"workdir\" string=\"true\">/repo</｜｜｜ parameter>\n" +
		"</｜｜｜ invoke>\n" +
		"</｜｜｜ tools>"
	p := NewParser()
	_, calls := p.Feed(text)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one", calls)
	}
	if calls[0].Name != "exec_command" {
		t.Fatalf("name = %q", calls[0].Name)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &got); err != nil {
		t.Fatalf("arguments %q: %v", calls[0].Arguments, err)
	}
	if got["cmd"] != "git status" || got["workdir"] != "/repo" {
		t.Fatalf("arguments = %#v", got)
	}
}

// Ordinary prose that merely contains fullwidth bars must stay visible.
func TestFeedLeavesFullwidthBarProseAlone(t *testing.T) {
	for _, text := range []string{
		"｜ normal | pipe text",
		"a ｜ b",
		"<｜｜ not a call>",
	} {
		p := NewParser()
		vis, calls := p.Feed(text)
		if len(calls) != 0 {
			t.Fatalf("%q produced calls %+v", text, calls)
		}
		if vis != text {
			t.Fatalf("visible = %q, want %q", vis, text)
		}
	}
}

// The byte-exact envelope captured from the route whose markup leaked into a
// Codex conversation.  It arrived as ordinary assistant text with tool_calls=0
// and finish_reason=stop, so Codex ended the turn and waited for the operator.
// Keeping the payload verbatim stops the pattern drifting from production.
func TestFeedParsesCapturedLeakedEnvelope(t *testing.T) {
	bar := "\uFF5C"
	sample := "<" + strings.Repeat(bar, 2) + " calls>\n" +
		"<" + strings.Repeat(bar, 2) + " invoke name=\"exec_command\">\n" +
		"<" + strings.Repeat(bar, 2) + " parameter name=\"cmd\" string=\"true\">git diff</" + strings.Repeat(bar, 3) + " parameter>\n" +
		"<" + strings.Repeat(bar, 3) + " parameter name=\"justification\" string=\"true\"></" + strings.Repeat(bar, 3) + ">\n" +
		"<" + strings.Repeat(bar, 3) + " parameter name=\"workdir\" string=\"true\">F:\\repo</" + strings.Repeat(bar, 3) + ">\n" +
		"</" + strings.Repeat(bar, 3) + " invoke>\n" +
		"</" + strings.Repeat(bar, 3) + " calls>"
	vis, calls := NewParser().Feed(sample)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v (visible %q), want one", calls, vis)
	}
	if calls[0].Name != "exec_command" {
		t.Fatalf("name = %q, want exec_command", calls[0].Name)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &got); err != nil {
		t.Fatalf("arguments %q: %v", calls[0].Arguments, err)
	}
	if got["cmd"] != "git diff" || got["justification"] != "" || got["workdir"] != `F:\repo` {
		t.Fatalf("arguments = %#v", got)
	}
	if vis != "" {
		t.Fatalf("visible text leaked: %q", vis)
	}
}

const truncBars = "\uFF5C"

// TestFinishParsesTruncatedBareInvokeEnvelope pins the captured shape that
// still leaked after the markerless fix: the envelope has no `<... calls>`
// wrapper, the stream simply stops after the last parameter, and a parameter
// may spell its name as the element's own attribute (`parameter="cmd"`)
// instead of `name="cmd"`. None of it forms a complete `<invoke>...</invoke>`,
// so the old parser saw plain prose and the turn ended with no tool call.
func TestFinishParsesTruncatedBareInvokeEnvelope(t *testing.T) {
	text := "\n" +
		"<" + truncBars + truncBars + " invoke name=\"exec_command\">\n" +
		"<" + truncBars + truncBars + " parameter=\"cmd\" string=\"true\">git status --short</" + truncBars + truncBars + truncBars + " parameter>\n" +
		"<" + truncBars + truncBars + truncBars + " parameter name=\"workdir\" string=\"true\">F:\\repo</" + truncBars + truncBars + truncBars + ">\n" +
		"<" + truncBars + truncBars + truncBars + " parameter name=\"yield_time_ms\" string=\"false\">180000</" + truncBars + truncBars + truncBars + ">\n"

	p := NewParser()
	vis, calls := p.Feed(text)
	if len(calls) != 0 {
		t.Fatalf("a truncated envelope must not be emitted mid-stream: %+v", calls)
	}
	// Whitespace before the envelope is ordinary prose; the markup itself
	// must not survive.
	if strings.TrimSpace(vis) != "" || strings.Contains(vis, truncBars) {
		t.Fatalf("a truncated envelope leaked as visible text: %q", vis)
	}
	tail, calls := p.Finish()
	if tail != "" {
		t.Fatalf("finish returned visible text: %q", tail)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one", calls)
	}
	if calls[0].Name != "exec_command" {
		t.Fatalf("name = %q", calls[0].Name)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &got); err != nil {
		t.Fatalf("arguments %q: %v", calls[0].Arguments, err)
	}
	want := map[string]string{"cmd": "git status --short", "workdir": "F:\\repo", "yield_time_ms": "180000"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("argument %q = %q, want %q (all: %#v)", k, got[k], v, got)
		}
	}
}

// TestFeedHoldsTruncatedBareInvokeUntilFinish arrives one rune at a time.
func TestFeedHoldsTruncatedBareInvokeUntilFinish(t *testing.T) {
	full := "<" + truncBars + truncBars + " invoke name=\"exec_command\">\n" +
		"<" + truncBars + truncBars + " parameter=\"cmd\" string=\"true\">ls</" + truncBars + truncBars + truncBars + " parameter>\n"
	p := NewParser()
	var leaked strings.Builder
	var calls []Call
	for _, r := range full {
		vis, got := p.Feed(string(r))
		leaked.WriteString(vis)
		calls = append(calls, got...)
	}
	if leaked.String() != "" {
		t.Fatalf("truncated envelope leaked as visible text: %q", leaked.String())
	}
	tail, finishCalls := p.Finish()
	calls = append(calls, finishCalls...)
	if tail != "" {
		t.Fatalf("finish returned visible text: %q", tail)
	}
	if len(calls) != 1 || calls[0].Name != "exec_command" || calls[0].Arguments != "{\"cmd\":\"ls\"}" {
		t.Fatalf("calls = %+v", calls)
	}
}

// TestFeedKeepsProseBeforeATruncatedEnvelope: prose before the envelope stays
// visible, and the call still comes back at Finish.
func TestFeedKeepsProseBeforeATruncatedEnvelope(t *testing.T) {
	text := "let me check.\n<" + truncBars + truncBars + " invoke name=\"exec_command\">\n" +
		"<" + truncBars + truncBars + " parameter=\"cmd\" string=\"true\">pwd</" + truncBars + truncBars + truncBars + " parameter>\n"
	p := NewParser()
	vis, _ := p.Feed(text)
	if vis != "let me check.\n" {
		t.Fatalf("visible = %q, want the prose before the envelope", vis)
	}
	_, calls := p.Finish()
	if len(calls) != 1 || calls[0].Arguments != "{\"cmd\":\"pwd\"}" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestRealLeakedTruncatedEnvelope(t *testing.T) {
	raw, err := os.ReadFile("testdata/truncated_fullwidth_envelope.txt")
	if err != nil {
		t.Skipf("sample not present: %v", err)
	}
	p := NewParser()
	if vis, calls := p.Feed(string(raw)); len(calls) != 0 || len(vis) > 2 {
		t.Fatalf("mid-stream: calls=%+v visible=%q", calls, vis)
	}
	tail, calls := p.Finish()
	if tail != "" {
		t.Fatalf("finish leaked back into visible text: %q", tail)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one", calls)
	}
	if calls[0].Name != "exec_command" {
		t.Fatalf("name = %q, want exec_command", calls[0].Name)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(calls[0].Arguments), &got); err != nil {
		t.Fatalf("arguments %q: %v", calls[0].Arguments, err)
	}
	want := map[string]string{
		"cmd":                 "git status --short; echo \"=== TSC ===\"; node node_modules/typescript/bin/tsc --noEmit 2>&1 | Select-Object -First 12; echo \"EXIT=$LASTEXITCODE\"",
		"justification":       "",
		"login":               "true",
		"max_output_tokens":   "6000",
		"sandbox_permissions": "use_default",
		"shell":               "powershell",
		"tty":                 "false",
		"workdir":             "F:\\aiNovelWriter\\repo",
		"yield_time_ms":       "180000",
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("argument %q = %q, want %q\nall: %#v", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d arguments, want %d: %#v", len(got), len(want), got)
	}
}
