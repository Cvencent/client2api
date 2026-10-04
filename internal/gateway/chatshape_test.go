package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// TestABufferedTurnAlwaysCarriesContent pins the shape of the assistant message
// the gateway builds for a non-streaming completion.  It is the one message the
// gateway constructs itself rather than relaying, and OpenAI's shape for it
// always carries `content` -- null when the turn produced tool calls and no
// prose, or nothing at all.  Omitting the key is not the same thing: a client
// that validates the message object, or that distinguishes "the model said
// nothing" from "the server forgot to say", sees a different shape from the one
// the same model returns through OpenAI directly.
//
// The prose case is the control: the fix must not turn a real answer into null.
func TestABufferedTurnAlwaysCarriesContent(t *testing.T) {
	cases := []struct {
		name   string
		events []core.Event
		// want is the expected content value; nil means the JSON literal null.
		want any
	}{
		{
			name: "prose",
			events: []core.Event{
				{Type: core.EventDelta, Delta: "pong"},
				{Type: core.EventDone, Finish: "stop"},
			},
			want: "pong",
		},
		{
			name: "reasoning only",
			events: []core.Event{
				{Type: core.EventDelta, Reasoning: "thinking about it"},
				{Type: core.EventDone, Finish: "stop"},
			},
			want: nil,
		},
		{
			name: "tool call only",
			events: []core.Event{
				{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{
					Index: 0, ID: "call_1", Name: "get_weather", Arguments: `{"city":"Paris"}`,
				}},
				{Type: core.EventDone, Finish: "tool_calls"},
			},
			want: nil,
		},
		{
			name:   "nothing at all",
			events: []core.Event{{Type: core.EventDone, Finish: "stop"}},
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, &testClient{name: "t", events: tc.events}, NewStats(), NewUsageStore(10))
			rec := chat(t, srv, bufferedBody)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}

			var got struct {
				Choices []struct {
					Message map[string]json.RawMessage `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if len(got.Choices) != 1 {
				t.Fatalf("choices = %d, want 1 (body %s)", len(got.Choices), rec.Body.String())
			}
			raw, ok := got.Choices[0].Message["content"]
			if !ok {
				t.Fatalf("the assistant message has no content key: %s", rec.Body.String())
			}
			if tc.want == nil {
				if string(raw) != "null" {
					t.Errorf("content = %s, want null", raw)
				}
				return
			}
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatalf("content = %s, want a JSON string", raw)
			}
			if s != tc.want {
				t.Errorf("content = %q, want %q", s, tc.want)
			}
		})
	}
}

// TestAToolCallTurnNeverEndsWithStop pins the finish_reason the gateway reports
// for a turn that produced tool calls.  OpenAI's meaning for "tool_calls" is
// exactly "the model called a tool", so "stop" beside a populated tool_calls
// array contradicts itself: a client that branches on finish_reason to decide
// whether to run the call behaves differently depending on which module served
// it.  Trae is the module that reports the transport-level stop while the
// message carries the calls, so the gateway owns the field and normalises it --
// in both the buffered and the streaming shape.
func TestAToolCallTurnNeverEndsWithStop(t *testing.T) {
	events := []core.Event{
		{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{
			Index: 0, ID: "call_1", Name: "get_weather", Arguments: `{"city":"Paris"}`,
		}},
		{Type: core.EventDone, Finish: "stop"},
	}

	t.Run("buffered", func(t *testing.T) {
		srv := newTestServer(t, &testClient{name: "t", events: events}, NewStats(), NewUsageStore(10))
		rec := chat(t, srv, bufferedBody)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		if len(got.Choices) != 1 {
			t.Fatalf("choices = %d, want 1", len(got.Choices))
		}
		if got.Choices[0].FinishReason != "tool_calls" {
			t.Errorf("finish_reason = %q, want tool_calls (body %s)", got.Choices[0].FinishReason, rec.Body.String())
		}
	})

	t.Run("streaming", func(t *testing.T) {
		srv := newTestServer(t, &testClient{name: "t", events: events}, NewStats(), NewUsageStore(10))
		rec := chat(t, srv, streamBody)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var last string
		for _, line := range strings.Split(rec.Body.String(), "\n") {
			payload, ok := strings.CutPrefix(line, "data: ")
			if !ok || payload == "[DONE]" {
				continue
			}
			var chunk struct {
				Choices []struct {
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				t.Fatalf("decode %q: %v", line, err)
			}
			if len(chunk.Choices) == 1 && chunk.Choices[0].FinishReason != nil {
				last = *chunk.Choices[0].FinishReason
			}
		}
		if last != "tool_calls" {
			t.Errorf("last finish_reason = %q, want tool_calls (body %s)", last, rec.Body.String())
		}
	})
}
