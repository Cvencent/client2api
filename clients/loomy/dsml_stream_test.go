package loomy

import (
	"encoding/json"
	"strings"
	"testing"

	"client2api/internal/core"
)

func TestStreamConvertsDSMLTextToToolCall(t *testing.T) {
	marker := "<" + strings.Repeat("\uFF5C", 2) + "DSML" + strings.Repeat("\uFF5C", 2)
	closeMarker := strings.TrimPrefix(marker, "<")
	content := "before\n" +
		marker + " calls>\n" +
		marker + ` invoke name="exec">` + "\n" +
		marker + ` parameter name="arguments" string="true">` +
		`{"cmd":"git status --short","workdir":"D:\\aipassport"}` +
		"</" + closeMarker + " parameter>\n" +
		"</" + closeMarker + " invoke>\n" +
		"</" + closeMarker + " calls>\n" +
		"after"

	frame := map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": content}}},
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Join([]string{
		"data: " + string(raw),
		"",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	events := drain(t, fixtureStream(t, body))
	var text strings.Builder
	var calls []core.ToolCallDelta
	var finish string
	for _, ev := range events {
		switch ev.Type {
		case core.EventDelta:
			text.WriteString(ev.Delta)
		case core.EventToolCall:
			if ev.ToolCall != nil {
				calls = append(calls, *ev.ToolCall)
			}
		case core.EventDone:
			finish = ev.Finish
		}
	}
	if got := text.String(); got != "before\n\nafter" {
		t.Fatalf("visible text = %q, want before and after", got)
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %+v, want one", calls)
	}
	if calls[0].Name != "exec" {
		t.Fatalf("tool call name = %q, want exec", calls[0].Name)
	}
	if calls[0].Arguments != `{"cmd":"git status --short","workdir":"D:\\aipassport"}` {
		t.Fatalf("tool call arguments = %q", calls[0].Arguments)
	}
	if finish != "stop" {
		t.Fatalf("finish = %q, want stop", finish)
	}
}
