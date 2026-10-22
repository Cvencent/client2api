package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

type toolMarkupTestStream struct {
	events []core.Event
	next   int
}

func (s *toolMarkupTestStream) Recv() (core.Event, error) {
	if s.next >= len(s.events) {
		return core.Event{}, io.EOF
	}
	ev := s.events[s.next]
	s.next++
	return ev, nil
}

func (s *toolMarkupTestStream) Close() error { return nil }

func dsmlTestEvents(text string, finish string) []core.Event {
	events := make([]core.Event, 0, len([]rune(text))+1)
	for _, r := range text {
		events = append(events, core.Event{Type: core.EventDelta, Delta: string(r)})
	}
	return append(events, core.Event{Type: core.EventDone, Finish: finish})
}

func TestToolMarkupStreamConvertsSplitDSMLIntoCall(t *testing.T) {
	marker := "<" + strings.Repeat("\uFF5C", 2) + "DSML" + strings.Repeat("\uFF5C", 2)
	closeMarker := strings.TrimPrefix(marker, "<")
	text := marker + " calls>\n" +
		marker + ` invoke name="exec">` + "\n" +
		marker + ` parameter name="arguments" string="true">{"cmd":"pwd"}</` + closeMarker + " parameter>\n" +
		"</" + closeMarker + " invoke>\n" +
		"</" + closeMarker + " calls>"

	stream := wrapToolMarkup(&toolMarkupTestStream{events: dsmlTestEvents(text, "stop")}, &core.ChatRequest{
		Tools: []core.Tool{{Type: "function", Name: "exec"}},
	})
	var visible strings.Builder
	var calls []*core.ToolCallDelta
	finish := ""
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch ev.Type {
		case core.EventDelta:
			visible.WriteString(ev.Delta)
		case core.EventToolCall:
			calls = append(calls, ev.ToolCall)
		case core.EventDone:
			finish = ev.Finish
		case core.EventError:
			t.Fatalf("unexpected protocol error: %v", ev.Err)
		}
	}
	if strings.Contains(visible.String(), "DSML") {
		t.Fatalf("raw DSML leaked as visible text: %q", visible.String())
	}
	if len(calls) != 1 || calls[0].Name != "exec" || calls[0].Arguments != `{"cmd":"pwd"}` {
		t.Fatalf("calls = %#v, want one exec call", calls)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls", finish)
	}
}

func TestToolMarkupStreamRejectsIncompleteDSML(t *testing.T) {
	marker := "<" + strings.Repeat("\uFF5C", 2) + "DSML" + strings.Repeat("\uFF5C", 2)
	text := marker + " calls>\n" + marker + ` invoke name="exec">`

	stream := wrapToolMarkup(&toolMarkupTestStream{events: dsmlTestEvents(text, "stop")}, &core.ChatRequest{
		Tools: []core.Tool{{Type: "function", Name: "exec"}},
	})
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			t.Fatal("incomplete DSML ended without an error event")
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Type == core.EventDelta && strings.Contains(ev.Delta, "DSML") {
			t.Fatalf("incomplete DSML leaked as visible text: %q", ev.Delta)
		}
		if ev.Type == core.EventError {
			return
		}
	}
}

func TestToolMarkupStreamDoesNotInterpretMarkupWithoutToolChoice(t *testing.T) {
	events := []core.Event{
		{Type: core.EventDelta, Delta: `<tool_calls>example</tool_calls>`},
		{Type: core.EventDone, Finish: "stop"},
	}
	for _, req := range []*core.ChatRequest{
		nil,
		{ToolChoice: []byte(`"none"`)},
	} {
		stream := wrapToolMarkup(&toolMarkupTestStream{events: append([]core.Event(nil), events...)}, req)
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Type != core.EventDelta || ev.Delta != `<tool_calls>example</tool_calls>` {
			t.Fatalf("event = %#v, want the original visible text", ev)
		}
	}
}

func TestChatCompletionsBufferedDSMLBecomesToolCalls(t *testing.T) {
	marker := "<" + strings.Repeat("\uFF5C", 2) + "DSML" + strings.Repeat("\uFF5C", 2)
	closeMarker := strings.TrimPrefix(marker, "<")
	dsml := marker + " calls>\n" +
		marker + ` invoke name="exec">` + "\n" +
		marker + ` parameter name="arguments" string="true">{"cmd":"pwd"}</` + closeMarker + " parameter>\n" +
		"</" + closeMarker + " invoke>\n" +
		"</" + closeMarker + " calls>"

	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventDelta, Delta: dsml[:len(dsml)/2]},
		{Type: core.EventDelta, Delta: dsml[len(dsml)/2:]},
		{Type: core.EventDone, Finish: "stop"},
	}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
	rec := chat(t, srv, `{"model":"m1","messages":[{"role":"user","content":"run it"}],"tools":[{"type":"function","function":{"name":"exec","parameters":{"type":"object","properties":{}}}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "DSML") {
		t.Fatalf("raw DSML leaked to the caller: %s", rec.Body)
	}
	var got chatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body = %s", err, rec.Body)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message == nil {
		t.Fatalf("choices = %#v", got.Choices)
	}
	calls := got.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].Function.Name != "exec" || calls[0].Function.Arguments != `{"cmd":"pwd"}` {
		t.Fatalf("tool_calls = %#v", calls)
	}
}
