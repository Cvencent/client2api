package workbuddy

import (
	"strings"
	"testing"
)

func TestDSMLParserDoubleBarArgumentsEnvelope(t *testing.T) {
	marker := "<" + strings.Repeat("\uFF5C", 2) + "DSML" + strings.Repeat("\uFF5C", 2)
	closeMarker := strings.TrimPrefix(marker, "<")
	full := marker + " calls>\n" +
		marker + ` invoke name="exec">` + "\n" +
		marker + ` parameter name="arguments" string="true">` +
		`{"cmd":"git status --short","workdir":"D:\\aipassport","timeout_ms":10000}` +
		"</" + closeMarker + " parameter>\n" +
		"</" + closeMarker + " invoke>\n" +
		"</" + closeMarker + " calls>"

	p := NewDSMLParser()
	text, calls := p.Feed(full)
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want 1", calls)
	}
	if calls[0].Name != "exec" {
		t.Fatalf("call name = %q, want exec", calls[0].Name)
	}
	if calls[0].Arguments != `{"cmd":"git status --short","workdir":"D:\\aipassport","timeout_ms":10000}` {
		t.Fatalf("arguments = %q", calls[0].Arguments)
	}
}
