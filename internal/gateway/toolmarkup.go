package gateway

import (
	"encoding/json"
	"errors"
	"strings"

	"client2api/internal/core"
	"client2api/internal/dsml"
)

// toolMarkupStream normalises models that emit DSML/XML tool-call syntax as
// visible content instead of returning structured tool calls.  It sits at the
// gateway boundary so every module gets the same treatment before either wire
// emitter (Chat Completions or Responses) sees the turn.
type toolMarkupStream struct {
	core.Stream
	parser        *dsml.Parser
	pending       []core.Event
	pendingCalls  []dsml.Call
	nextToolIndex int
}

func wrapToolMarkup(stream core.Stream, req *core.ChatRequest) core.Stream {
	if stream == nil || !requestCanUseTools(req) {
		return stream
	}
	return &toolMarkupStream{Stream: stream, parser: dsml.NewParser()}
}

// requestCanUseTools keeps ordinary prose containing XML-looking examples from
// being interpreted as calls when the caller did not offer any tools.
func requestCanUseTools(req *core.ChatRequest) bool {
	if req == nil || len(req.Tools) == 0 {
		return false
	}
	if len(req.ToolChoice) == 0 {
		return true
	}
	var choice any
	if json.Unmarshal(req.ToolChoice, &choice) != nil {
		return true
	}
	if name, ok := choice.(string); ok && strings.EqualFold(strings.TrimSpace(name), "none") {
		return false
	}
	return true
}

func (s *toolMarkupStream) Recv() (core.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}

		ev, err := s.Stream.Recv()
		if err != nil {
			return ev, err
		}

		switch ev.Type {
		case core.EventDelta:
			text, calls := s.parser.Feed(ev.Delta)
			if s.parser.Malformed() {
				return malformedToolMarkupEvent(), nil
			}
			ev.Delta = text
			if ev.Delta != "" || ev.Reasoning != "" {
				s.pending = append(s.pending, ev)
			}
			s.pendingCalls = append(s.pendingCalls, calls...)
		case core.EventToolCall:
			if ev.ToolCall != nil && ev.ToolCall.Index >= s.nextToolIndex {
				s.nextToolIndex = ev.ToolCall.Index + 1
			}
			return ev, nil
		case core.EventDone:
			text, calls := s.parser.Finish()
			if s.parser.Malformed() {
				return malformedToolMarkupEvent(), nil
			}
			if text != "" {
				s.pending = append(s.pending, core.Event{Type: core.EventDelta, Delta: text})
			}
			s.pendingCalls = append(s.pendingCalls, calls...)
			if len(s.pendingCalls) > 0 {
				for _, call := range s.pendingCalls {
					s.pending = append(s.pending, core.Event{
						Type: core.EventToolCall,
						ToolCall: &core.ToolCallDelta{
							Index:     s.nextToolIndex,
							Name:      call.Name,
							Arguments: call.Arguments,
						},
					})
					s.nextToolIndex++
				}
				s.pendingCalls = nil
				if ev.Finish == "" || ev.Finish == "stop" {
					ev.Finish = "tool_calls"
				}
			}
			s.pending = append(s.pending, ev)
		default:
			return ev, nil
		}
	}
}

func malformedToolMarkupEvent() core.Event {
	return core.Event{
		Type: core.EventError,
		Err:  errors.New("upstream returned malformed or incomplete tool-call markup"),
	}
}
