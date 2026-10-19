package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"client2api/internal/core"
)

// observedRow drives one request through the given protocol and returns the
// single usage row it produced.  protocol is "chat" or "responses".
func observedRow(t *testing.T, events []core.Event, protocol string, stream bool) UsageRecord {
	t.Helper()
	usage := NewUsageStore(10)
	srv := newTestServer(t, &testClient{name: "t", events: events}, NewStats(), usage)

	var path, body string
	if protocol == "responses" {
		path = "/v1/responses"
		b, err := json.Marshal(map[string]any{"model": "t/m1", "input": "hi", "stream": stream})
		if err != nil {
			t.Fatal(err)
		}
		body = string(b)
	} else {
		path = "/v1/chat/completions"
		body = fmt.Sprintf(`{"model":"t/m1","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
	}

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	rows := usage.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("%s stream=%v: usage rows = %d, want exactly one", protocol, stream, len(rows))
	}
	return rows[0]
}

// TestFailedTurnNamesItsReason pins an operator-facing explanation onto every
// way a request can fail after its stream opened.  These paths used to call
// s.fail(rec) without a cause, so the row was marked failed while
// FailureReason stayed empty and the panel's 失败原因 column rendered "-".
// The console log had the answer, but the row an operator actually reads did
// not.
func TestFailedTurnNamesItsReason(t *testing.T) {
	moduleErr := errors.New("autoclaw: upstream stopped sending data (idle timeout)")
	cases := []struct {
		name   string
		events []core.Event
		// want is a substring: the full reason also carries the vendor's own
		// message, which this test deliberately does not pin.
		want string
	}{
		{
			name: "stream truncated before its terminal frame",
			events: []core.Event{
				{Type: core.EventDelta, Reasoning: "thinking about it"},
				{Type: core.EventError, Err: io.ErrUnexpectedEOF},
			},
			want: "上游流提前中断",
		},
		{
			// The module reported the real cause and then closed the stream;
			// the trailing io.EOF must not flatten it to "unexpected EOF".
			name:   "module's own error survives the closing EOF",
			events: []core.Event{{Type: core.EventError, Err: moduleErr}},
			want:   moduleErr.Error(),
		},
	}

	for _, protocol := range []string{"chat", "responses"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(tc.name+"/"+protocol+"/stream="+strconv.FormatBool(stream), func(t *testing.T) {
					row := observedRow(t, tc.events, protocol, stream)
					if !row.Failed {
						t.Fatalf("row was not marked failed: %+v", row)
					}
					if !strings.Contains(row.FailureReason, tc.want) {
						t.Fatalf("FailureReason = %q, want it to name %q", row.FailureReason, tc.want)
					}
				})
			}
		}
	}
}

// TestRejectedRequestNamesItsReason covers the two pre-module exits: a body
// that is not JSON, and a body that parses but does not translate into a
// request.  Both were marked failed with no reason, so an operator saw a row
// with status 400 and an empty 失败原因.
func TestRejectedRequestNamesItsReason(t *testing.T) {
	// Exactly which validation error wins belongs to toCoreRequest*; what this
	// test owns is that whichever one it is reaches the row.
	bodies := []struct {
		name string
		body string
	}{
		{name: "body is not JSON", body: "{not json"},
		{name: "body carries no usable request", body: `{"input":"hi"}`},
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, tc := range bodies {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				usage := NewUsageStore(10)
				srv := newTestServer(t, &testClient{name: "t"}, NewStats(), usage)
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				srv.Handler.ServeHTTP(httptest.NewRecorder(), req)

				rows := usage.Snapshot()
				if len(rows) != 1 {
					t.Fatalf("usage rows = %d, want exactly one", len(rows))
				}
				if !rows[0].Failed {
					t.Fatalf("row was not marked failed: %+v", rows[0])
				}
				if strings.TrimSpace(rows[0].FailureReason) == "" {
					t.Fatalf("FailureReason is empty for a rejected request: %+v", rows[0])
				}
			})
		}
	}
}

// TestEmptyTurnNamesItsReason covers the Responses protocol's dead-turn
// verdict, which the Chat protocol does not share: only Responses turns an
// empty terminal frame into a failure, and that failure must be labelled too.
func TestEmptyTurnNamesItsReason(t *testing.T) {
	events := []core.Event{
		{Type: core.EventDelta, Reasoning: "thinking, but never answering"},
		{Type: core.EventDone, Finish: "stop"},
	}
	for _, stream := range []bool{false, true} {
		t.Run("responses/stream="+strconv.FormatBool(stream), func(t *testing.T) {
			row := observedRow(t, events, "responses", stream)
			if !row.Failed {
				t.Fatalf("an empty turn was not marked failed: %+v", row)
			}
			if !strings.Contains(row.FailureReason, "上游返回空回合") {
				t.Fatalf("FailureReason = %q, want it to name the empty turn", row.FailureReason)
			}
		})
	}
}
