package workbuddy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

const terminalMessageToolCallFixture = `data: {"id":"chatcmpl-terminal","object":"chat.completion.chunk","created":1,"model":"space-bunny","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me write."}}]}

data: {"id":"chatcmpl-terminal","object":"chat.completion.chunk","created":1,"model":"space-bunny","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"index":0,"id":"call_terminal","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"/tmp/out\"}"}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]
`

func TestChatEmitsToolCallFromTerminalMessageAfterContent(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return sseResponse(200, terminalMessageToolCallFixture, nil), nil
	}}
	c, _ := newTestClient(t, rt)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "space-bunny",
		User:     "conv-terminal",
		Messages: []core.Message{{Role: "user", Content: "write a file"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	var (
		text   strings.Builder
		calls  []core.ToolCallDelta
		finish string
	)
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch ev.Type {
		case core.EventDelta:
			text.WriteString(ev.Delta)
		case core.EventToolCall:
			if ev.ToolCall == nil {
				t.Fatal("tool call event without payload")
			}
			calls = append(calls, *ev.ToolCall)
		case core.EventDone:
			finish = ev.Finish
		case core.EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}

	if got := text.String(); got != "Let me write." {
		t.Fatalf("text = %q", got)
	}
	merged := mergeToolFragments(calls)
	if len(merged) != 1 {
		t.Fatalf("tool calls = %+v (raw %+v)", merged, calls)
	}
	if merged[0].ID != "call_terminal" || merged[0].Name != "write_file" ||
		merged[0].Arguments != `{"path":"/tmp/out"}` {
		t.Fatalf("tool call = %+v", merged[0])
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %q", finish)
	}
}

func TestAggregateKeepsTerminalMessageToolCallAfterContent(t *testing.T) {
	msg, err := Aggregate(strings.NewReader(terminalMessageToolCallFixture))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choice := msg["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if got := message["content"]; got != "Let me write." {
		t.Fatalf("content = %#v", got)
	}
	if message["tool_calls"] == nil {
		t.Fatal("Aggregate dropped the terminal message tool call")
	}
}
