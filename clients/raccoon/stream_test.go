package raccoon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---- helpers ----------------------------------------------------------

func writeSSE(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for _, f := range frames {
		fmt.Fprintf(w, "data: %s\n\n", f)
		if fl != nil {
			fl.Flush()
		}
	}
}

func drain(t *testing.T, s core.Stream) []core.Event {
	t.Helper()
	defer s.Close()
	var out []core.Event
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		out = append(out, ev)
	}
}

func textOf(events []core.Event) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Type == core.EventDelta {
			b.WriteString(ev.Delta)
		}
	}
	return b.String()
}

func toolCallsOf(events []core.Event) []*core.ToolCallDelta {
	var out []*core.ToolCallDelta
	for _, ev := range events {
		if ev.Type == core.EventToolCall && ev.ToolCall != nil {
			out = append(out, ev.ToolCall)
		}
	}
	return out
}

func doneEvent(t *testing.T, events []core.Event) core.Event {
	t.Helper()
	for _, ev := range events {
		if ev.Type == core.EventDone {
			return ev
		}
	}
	t.Fatalf("no done event in %d events", len(events))
	return core.Event{}
}

// ---- SSE framing ------------------------------------------------------

func TestReadFramesFraming(t *testing.T) {
	body := strings.Join([]string{
		": this is a comment / heartbeat",
		"",
		"data: {\"a\":1}",
		"",
		"",
		"data: {\"n\":",
		"data: 1}",
		"",
		"event: ping",
		"id: 7",
		"retry: 100",
		"data: {\"b\":2}",
		"",
		"{\"c\":3}",
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var got []string
	err := readFrames(context.Background(), strings.NewReader(body), func(p string) error {
		got = append(got, p)
		return nil
	})
	if err != nil {
		t.Fatalf("readFrames: %v", err)
	}
	want := []string{`{"a":1}`, "{\"n\":\n1}", `{"b":2}`, `{"c":3}`, `[DONE]`}
	if len(got) != len(want) {
		t.Fatalf("got %d payloads %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("payload[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReadFramesEOFIsClean(t *testing.T) {
	// A truncated final event (no trailing blank line) must still be
	// delivered, and the end of the body must not be an error.
	var got []string
	err := readFrames(context.Background(), strings.NewReader("data: {\"z\":9}"), func(p string) error {
		got = append(got, p)
		return nil
	})
	if err != nil {
		t.Fatalf("io.EOF at the end of a stream must be clean, got %v", err)
	}
	if len(got) != 1 || got[0] != `{"z":9}` {
		t.Fatalf("got %q, want the truncated final frame flushed", got)
	}
}

func TestReadFramesHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := readFrames(ctx, strings.NewReader("data: {\"a\":1}\n\n"), func(string) error { return nil })
	if err == nil {
		t.Fatal("a cancelled context must stop the reader")
	}
}

// ---- chunk parsing ----------------------------------------------------

func TestParseChunkShapes(t *testing.T) {
	c, ok := parseChunk(`{"choices":[{"delta":{"content":"he"}}]}`)
	if !ok || c.Delta != "he" {
		t.Fatalf("content delta: %#v ok=%v", c, ok)
	}
	c, ok = parseChunk(`{"choices":[{"delta":{"reasoning_content":"why"}}]}`)
	if !ok || c.Reasoning != "why" {
		t.Fatalf("reasoning delta: %#v ok=%v", c, ok)
	}
	c, ok = parseChunk(`{"choices":[{"delta":{"reasoning":"why2"}}]}`)
	if !ok || c.Reasoning != "why2" {
		t.Fatalf("reasoning alias: %#v ok=%v", c, ok)
	}
	c, ok = parseChunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`)
	if !ok || len(c.ToolCalls) != 1 || c.ToolCalls[0].ID != "c1" || c.ToolCalls[0].Name != "f" || c.ToolCalls[0].Arguments != "{}" {
		t.Fatalf("tool call delta: %#v ok=%v", c, ok)
	}
	c, ok = parseChunk(`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"x"}},{"function":{"arguments":"y"}}]}}]}`)
	if !ok || len(c.ToolCalls) != 2 || c.ToolCalls[0].Index != 0 || c.ToolCalls[1].Index != 1 {
		t.Fatalf("a missing index must default to the slice position: %#v", c.ToolCalls)
	}
	c, ok = parseChunk(`{"choices":[{"finish_reason":"stop"}]}`)
	if !ok || c.Finish != "stop" {
		t.Fatalf("finish reason: %#v ok=%v", c, ok)
	}
	c, ok = parseChunk(`{"choices":[{"message":{"content":"whole"}}]}`)
	if !ok || c.Delta != "whole" {
		t.Fatalf("a whole non-streamed message must be read: %#v ok=%v", c, ok)
	}
	c, ok = parseChunk(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
	if !ok || c.Usage == nil || c.Usage.PromptTokens != 10 || c.Usage.CompletionTokens != 2 || c.Usage.TotalTokens != 12 {
		t.Fatalf("usage: %#v ok=%v", c.Usage, ok)
	}
	c, ok = parseChunk(`{"error":{"message":"boom"}}`)
	if !ok || c.Err != "boom" {
		t.Fatalf("error object: %#v ok=%v", c, ok)
	}
	c, ok = parseChunk(`{"code":200003,"message":"please log in again"}`)
	if !ok || c.Err != "please log in again" {
		t.Fatalf("envelope error: %#v ok=%v", c, ok)
	}
	for _, raw := range []string{"", "[DONE]", "{}", "null", "not json", `{"choices":[]}`, `{"hello":"world"}`} {
		if _, ok := parseChunk(raw); ok {
			t.Errorf("parseChunk(%q) must report nothing to do", raw)
		}
	}
}

func TestNormalizeFinish(t *testing.T) {
	cases := []struct {
		in      string
		sawTool bool
		want    string
	}{
		{"stop", false, "stop"},
		{"length", false, "length"},
		{"tool_calls", false, "tool_calls"},
		{"content_filter", false, "content_filter"},
		{"weird", true, "tool_calls"},
		{"", true, "tool_calls"},
		{"weird", false, "stop"},
		{"", false, "stop"},
	}
	for _, c := range cases {
		if got := normalizeFinish(c.in, c.sawTool); got != c.want {
			t.Errorf("normalizeFinish(%q, %v) = %q, want %q", c.in, c.sawTool, got, c.want)
		}
	}
}

// ---- end-to-end chat --------------------------------------------------

func TestChatStreamMergesToolCallFragments(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathChat {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-live" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Org-Code"); got != "org-9x" {
			t.Errorf("X-Org-Code = %q, want the org code forwarded", got)
		}
		writeSSE(w,
			`{"choices":[{"delta":{"content":"let me "}}]}`,
			`{"choices":[{"delta":{"content":"check"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"SH\"}"}}]}}]}`,
			`{"choices":[{"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":9,"total_tokens":16}}`,
			`[DONE]`,
		)
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q,"access_token":"tok-live","office_identity":"org-9x"}`, ts.URL), ts.Client())
	s, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "sn-kimi-k3 · x1",
		Messages: []core.Message{{Role: "user", Content: "weather?"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := drain(t, s)
	if got := textOf(events); got != "let me check" {
		t.Errorf("text = %q", got)
	}
	calls := toolCallsOf(events)
	if len(calls) != 1 {
		t.Fatalf("got %d tool_call events, want exactly 1 (fragments must merge, not leak)", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "get_weather" {
		t.Errorf("tool call = %#v", calls[0])
	}
	if calls[0].Arguments != `{"city":"SH"}` {
		t.Errorf("arguments = %q, want the two fragments concatenated", calls[0].Arguments)
	}
	var usage *core.Usage
	for _, ev := range events {
		if ev.Type == core.EventUsage {
			usage = ev.Usage
		}
	}
	if usage == nil || usage.PromptTokens != 7 || usage.CompletionTokens != 9 || usage.TotalTokens != 16 {
		t.Errorf("usage = %#v", usage)
	}
	if got := doneEvent(t, events).Finish; got != "tool_calls" {
		t.Errorf("finish = %q", got)
	}
}

func TestChatStreamReasoningBeforeDelta(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			`{"choices":[{"delta":{"reasoning_content":"hmm"}}]}`,
			`{"choices":[{"delta":{"content":"answer"}}]}`,
			`{"choices":[{"finish_reason":"stop"}]}`,
			`[DONE]`,
		)
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q,"access_token":"tok"}`, ts.URL), ts.Client())
	s, err := c.Chat(context.Background(), &core.ChatRequest{Model: "sn-kimi-k3", Messages: []core.Message{{Role: "user", Content: "q"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := drain(t, s)
	var reasoning string
	for _, ev := range events {
		if ev.Type == core.EventDelta {
			reasoning += ev.Reasoning
		}
	}
	if reasoning != "hmm" {
		t.Errorf("reasoning = %q", reasoning)
	}
	if got := textOf(events); got != "answer" {
		t.Errorf("text = %q", got)
	}
	if got := doneEvent(t, events).Finish; got != "stop" {
		t.Errorf("finish = %q", got)
	}
}

// TestChatReframesWholeJSONCompletion covers a gateway that ignores
// `stream:true` and answers with one whole JSON body.
func TestChatReframesWholeJSONCompletion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"code":0,"data":{"choices":[{"message":{"content":"whole answer"},"finish_reason":"stop"}]}}`)
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q,"access_token":"tok"}`, ts.URL), ts.Client())
	s, err := c.Chat(context.Background(), &core.ChatRequest{Model: "sn-kimi-k3", Messages: []core.Message{{Role: "user", Content: "q"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := drain(t, s)
	if got := textOf(events); got != "whole answer" {
		t.Errorf("text = %q, want the whole message re-framed as one delta", got)
	}
	if got := doneEvent(t, events).Finish; got != "stop" {
		t.Errorf("finish = %q", got)
	}
}

func TestChatStreamIdleTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"start"}}]}`)
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(),
		fmt.Sprintf(`{"base_url":%q,"access_token":"tok","first_token_timeout":"2s","idle_timeout":"150ms"}`, ts.URL), ts.Client())
	s, err := c.Chat(context.Background(), &core.ChatRequest{Model: "sn-kimi-k3", Messages: []core.Message{{Role: "user", Content: "q"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer s.Close()

	deadline := time.After(5 * time.Second)
	var sawErr error
	for {
		select {
		case <-deadline:
			t.Fatal("the idle timeout never fired")
		default:
		}
		ev, err := s.Recv()
		if err != nil {
			t.Fatalf("Recv: %v (want an error EVENT, not a transport error)", err)
		}
		if ev.Type == core.EventError {
			sawErr = ev.Err
			break
		}
		if ev.Type == core.EventDone {
			t.Fatal("the stream finished instead of timing out")
		}
	}
	if sawErr == nil || !strings.Contains(sawErr.Error(), "idle timeout") {
		t.Fatalf("idle timeout error = %v", sawErr)
	}
}

func TestChatEmptyStreamIsAnError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q,"access_token":"tok"}`, ts.URL), ts.Client())
	s, err := c.Chat(context.Background(), &core.ChatRequest{Model: "sn-kimi-k3", Messages: []core.Message{{Role: "user", Content: "q"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := drain(t, s)
	if len(events) != 1 || events[0].Type != core.EventError {
		t.Fatalf("a stream with no data must emit exactly one error event, got %#v", events)
	}
}

// ---- refresh ----------------------------------------------------------

func TestRefreshRequestShape(t *testing.T) {
	var gotBody map[string]any
	newTok := tokenExpiringIn(t, 3*time.Hour)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathRefresh {
			t.Errorf("path = %q, want %q", r.URL.Path, pathRefresh)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer old-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Org-Code"); got != "org-9x" {
			t.Errorf("X-Org-Code = %q, want the org code forwarded", got)
		}
		if got := r.Header.Get("X-Raccoon-Language"); got != "zh" {
			t.Errorf("X-Raccoon-Language = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, fmt.Sprintf(`{"code":0,"message":"","data":{"access_token":%q,"nickname":"nick","office_identity":"personal","user_id":"u-1"}}`, newTok))
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q,"access_token":"old-token"}`, ts.URL), ts.Client())
	res, err := c.refresh(context.Background(), credential{
		AccessToken:    "old-token",
		RefreshToken:   "RT-1",
		OfficeIdentity: "org-9x",
		DeviceID:       "dev-1",
		Nickname:       "old-nick",
	})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if gotBody["refresh_token"] != "RT-1" {
		t.Errorf("request body = %#v, want refresh_token RT-1", gotBody)
	}
	if string(res.AccessToken) != newTok {
		t.Errorf("access_token = %q", res.AccessToken)
	}
	// The response carried NO refresh_token, so the old one must be KEPT:
	// dropping it would make the account unrefreshable after one rotation.
	if string(res.RefreshToken) != "RT-1" {
		t.Errorf("refresh_token = %q, want the old one kept", res.RefreshToken)
	}
	got := credential{AccessToken: res.AccessToken, ExpiresAt: res.ExpiresAt}
	if ms, ok := got.expiresAtMs(); !ok || ms < time.Now().Add(2*time.Hour).UnixMilli() {
		t.Errorf("expires_at = %q (%d, %v), want the NEW jwt exp", res.ExpiresAt, ms, ok)
	}
}

func TestRefreshDeadToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"code":200003,"message":"please log in again"}`)
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q,"access_token":"old"}`, ts.URL), ts.Client())
	_, err := c.refresh(context.Background(), credential{AccessToken: "old", RefreshToken: "RT-dead"})
	if err == nil {
		t.Fatal("a rejected refresh token must be an error")
	}
	if !errors.Is(err, errSessionDead) {
		t.Fatalf("err = %v, want it to wrap errSessionDead so the pool can park the account", err)
	}
}

func TestRefreshAccountWritesBackAndKeepsOldToken(t *testing.T) {
	newTok := tokenExpiringIn(t, 3*time.Hour)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, fmt.Sprintf(`{"code":0,"data":{"access_token":%q}}`, newTok))
	}))
	defer ts.Close()

	dir := t.TempDir()
	c := newTestClient(t, dir, fmt.Sprintf(`{"base_url":%q,"access_token":"old","refresh_token":"RT-1","user_id":"u-1"}`, ts.URL), ts.Client())
	res, err := c.RefreshAccount(context.Background(), "")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("RefreshAccount = %#v", res)
	}
	e := c.pool.find("uid:u-1")
	if e == nil {
		t.Fatal("account vanished from the pool")
	}
	if string(e.acct.AccessToken) != newTok {
		t.Errorf("pool token = %q, want the new one", e.acct.AccessToken)
	}
	if string(e.acct.RefreshToken) != "RT-1" {
		t.Errorf("pool refresh token = %q, want the old one kept", e.acct.RefreshToken)
	}
	if ms, ok := e.acct.cred().expiresAtMs(); !ok || ms < time.Now().Add(2*time.Hour).UnixMilli() {
		t.Errorf("pool expiry = %d ok=%v, want the new jwt exp written back", ms, ok)
	}
	// And it must be persisted, not just held in memory.
	raw, err := os.ReadFile(filepath.Join(dir, accountsFile))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if !strings.Contains(string(raw), newTok) {
		t.Error("the refreshed token was not persisted to the account store")
	}
}

// TestRefreshTokenCarryOver guards the `refresh` line itself. The pool merges
// only non-empty fields, so the end-to-end test above still passes if
// `refresh` blanks the refresh token; the consequence is nonetheless real —
// an account whose refresh token was blanked can never renew again.
func TestRefreshTokenCarryOver(t *testing.T) {
	newTok := tokenExpiringIn(t, 3*time.Hour)

	serve := func(data string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, data)
		}))
	}

	t.Run("omitted keeps the old token", func(t *testing.T) {
		ts := serve(fmt.Sprintf(`{"code":0,"data":{"access_token":%q}}`, newTok))
		defer ts.Close()
		c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q}`, ts.URL), ts.Client())
		res, err := c.refresh(context.Background(), credential{AccessToken: "old", RefreshToken: "RT-1"})
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if res.AccessToken != newTok {
			t.Errorf("access token = %q, want the new one", res.AccessToken)
		}
		if res.RefreshToken != "RT-1" {
			t.Errorf("refresh token = %q, want the old one kept", res.RefreshToken)
		}
		if exp := jwtExpiryMs(res.AccessToken); exp <= 0 || string(res.ExpiresAt) != strconv.FormatInt(exp, 10) {
			t.Errorf("expires_at = %q, want the new token's jwt exp %d", res.ExpiresAt, exp)
		}
	})

	t.Run("a new token replaces the old one", func(t *testing.T) {
		ts := serve(fmt.Sprintf(`{"code":0,"data":{"access_token":%q,"refresh_token":"RT-2"}}`, newTok))
		defer ts.Close()
		c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q}`, ts.URL), ts.Client())
		res, err := c.refresh(context.Background(), credential{AccessToken: "old", RefreshToken: "RT-1"})
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if res.RefreshToken != "RT-2" {
			t.Errorf("refresh token = %q, want RT-2", res.RefreshToken)
		}
	})

	t.Run("a refresh with no token is refused locally", func(t *testing.T) {
		c := newTestClient(t, t.TempDir(), `{}`, nil)
		if _, err := c.refresh(context.Background(), credential{AccessToken: "old"}); err == nil {
			t.Error("refresh without a refresh_token must fail without a request")
		}
	})
}

// TestRefreshDueIgnoresTheEnabledFlag pins the reference's headline defect:
// its renewal sweep once filtered on `enabled`, so two disabled accounts'
// refresh tokens silently lapsed. Disabling affects automatic selection, not
// whether a credential has to stay fresh.
func TestRefreshDueIgnoresTheEnabledFlag(t *testing.T) {
	soon := tokenExpiringIn(t, 60*time.Second)
	cfg := fmt.Sprintf(`{"accounts":[
		{"label":"on","access_token":%q,"refresh_token":"RT-A","user_id":"u-on"},
		{"label":"off","access_token":%q,"refresh_token":"RT-B","user_id":"u-off","disabled":true},
		{"label":"norefresh","access_token":%q,"user_id":"u-nr"}
	]}`, soon, soon, soon)
	c := newTestClient(t, t.TempDir(), cfg, nil)

	due := map[string]bool{}
	for _, e := range c.pool.refreshDue(300 * time.Second) {
		due[e.acct.id()] = true
	}
	if !due["uid:u-on"] {
		t.Error("an enabled account inside the window must be due")
	}
	if !due["uid:u-off"] {
		t.Error("a DISABLED account inside the window must still be due")
	}
	if due["uid:u-nr"] {
		t.Error("an account with no refresh token can never be due")
	}
	// And the enabled flag must still be respected for SELECTION.
	if e := c.pool.find("uid:u-off"); e == nil || !e.acct.Disabled {
		t.Error("the disabled account must stay disabled in the pool")
	}
}

func TestChatRetriesOnceAfterAuthFailure(t *testing.T) {
	newTok := tokenExpiringIn(t, 3*time.Hour)
	var chatCalls, refreshCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathRefresh:
			refreshCalls++
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, fmt.Sprintf(`{"code":0,"data":{"access_token":%q,"refresh_token":"RT-2"}}`, newTok))
		case pathChat:
			chatCalls++
			if got := r.Header.Get("Authorization"); got == "Bearer old" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"code":200003,"message":"token expired"}`)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer "+newTok {
				t.Errorf("retry Authorization = %q, want the refreshed token", got)
			}
			writeSSE(w, `{"choices":[{"delta":{"content":"ok"}}]}`, `{"choices":[{"finish_reason":"stop"}]}`, `[DONE]`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(),
		fmt.Sprintf(`{"base_url":%q,"access_token":"old","refresh_token":"RT-1"}`, ts.URL), ts.Client())
	s, err := c.Chat(context.Background(), &core.ChatRequest{Model: "sn-kimi-k3", Messages: []core.Message{{Role: "user", Content: "q"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := drain(t, s)
	if got := textOf(events); got != "ok" {
		t.Errorf("text = %q", got)
	}
	if chatCalls != 2 {
		t.Errorf("chat calls = %d, want exactly 2 (one original + one retry)", chatCalls)
	}
	if refreshCalls != 1 {
		t.Errorf("refresh calls = %d, want exactly 1", refreshCalls)
	}
}

// TestTestAccountSendsARealChatRequest pins the change the operator asked for:
// the panel's 测试 button must send a real completion, not just read the
// user-info endpoint.
func TestTestAccountSendsARealChatRequest(t *testing.T) {
	var chatCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathChat {
			t.Errorf("unexpected path %q", r.URL.Path)
			return
		}
		chatCalls++
		writeSSE(w, `{"choices":[{"delta":{"content":"pong"}}]}`, `{"choices":[{"finish_reason":"stop"}]}`, `[DONE]`)
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(),
		fmt.Sprintf(`{"base_url":%q,"access_token":"tok","user_id":"u1"}`, ts.URL), ts.Client())
	res, err := c.TestAccount(context.Background(), "uid:u1")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount failed: %+v", res)
	}
	if res.Reply != "pong" {
		t.Fatalf("Reply = %q, want the streamed text", res.Reply)
	}
	if chatCalls != 1 {
		t.Fatalf("chat calls = %d, want 1", chatCalls)
	}
}

// ---- credential management -------------------------------------------

func TestChatNotConfigured(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{}`, nil)
	_, err := c.Chat(context.Background(), &core.ChatRequest{Model: "sn-kimi-k3"})
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want core.ErrNotConfigured", err)
	}
}

func TestChatRejectsAnEmptyModel(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{"access_token":"tok"}`, nil)
	if _, err := c.Chat(context.Background(), &core.ChatRequest{}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want core.ErrUnsupported (never a silent default model)", err)
	}
	if _, err := c.Chat(context.Background(), nil); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("nil request: err = %v, want core.ErrUnsupported", err)
	}
}

func TestStatusIsHonestAboutNoCredential(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{}`, nil)
	st := c.Status(context.Background())
	if st.Name != "raccoon" {
		t.Errorf("Name = %q", st.Name)
	}
	if st.Ready {
		t.Error("an unconfigured module must not report Ready")
	}
	if !strings.Contains(st.Detail, "access_token") {
		t.Errorf("Detail = %q, want it to name the config key to set", st.Detail)
	}
	if len(st.Accounts) != 0 {
		t.Errorf("Accounts = %#v, want none", st.Accounts)
	}
}

func TestStatusReadyWithACredential(t *testing.T) {
	tok := tokenExpiringIn(t, 3*time.Hour)
	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"access_token":%q,"user_id":"u-7"}`, tok), nil)
	st := c.Status(context.Background())
	if !st.Ready {
		t.Fatalf("Ready = false with a live credential: %s", st.Detail)
	}
	if len(st.Accounts) != 1 {
		t.Fatalf("Accounts = %#v", st.Accounts)
	}
	a := st.Accounts[0]
	if a.Identity != "u-7" {
		t.Errorf("Identity = %q, want the user id so two credentials group as one account", a.Identity)
	}
	if a.State != stateReady {
		t.Errorf("State = %q", a.State)
	}
	if a.ExpiresAt == "" {
		t.Error("ExpiresAt must be reported when the JWT carries an exp")
	}
	if len(st.Models) == 0 {
		t.Error("Status must list the built-in model ids")
	}
}

// TestAClientErrorNeverParksAHealthyAccount pins the fix for a defect a live
// run found in cline and which raccoon shared: a request-side error (a 400 the
// vendor rejected, or a 404 for a model it does not serve) parked a perfectly
// good credential, so the NEXT request — for a model that does work — was
// answered "no healthy account".  retryable() already refuses to rotate on a
// client error; the account that sent it is still healthy and must stay
// selectable.
func TestAClientErrorNeverParksAHealthyAccount(t *testing.T) {
	c := newTestClient(t, t.TempDir(),
		fmt.Sprintf(`{"access_token":%q,"user_id":"u-7"}`, tokenExpiringIn(t, 3*time.Hour)), nil)

	e := c.pool.pick(nil)
	if e == nil {
		t.Fatal("pick() returned nil for a live credential")
	}
	c.pool.markFailureWith(e, kindClient, "400 bad request", 0)

	if got := c.pool.usable(); len(got) != 1 {
		t.Fatalf("usable() = %d entries after a client error, want the account still selectable", len(got))
	}
	st := c.Status(context.Background())
	if len(st.Accounts) != 1 {
		t.Fatalf("Status.Accounts = %d rows, want 1", len(st.Accounts))
	}
	if got := st.Accounts[0].State; got != stateReady {
		t.Fatalf("state = %q after a client error, want %q", got, stateReady)
	}
	if !st.Ready {
		t.Fatal("Status reports not ready after a client error on a healthy account")
	}
}

// TestAnAuthErrorStillParksTheAccount is the other half: removing kindClient
// from the park arm must not weaken the arm that IS the credential's fault.
func TestAnAuthErrorStillParksTheAccount(t *testing.T) {
	c := newTestClient(t, t.TempDir(),
		fmt.Sprintf(`{"access_token":%q,"user_id":"u-7"}`, tokenExpiringIn(t, 3*time.Hour)), nil)

	e := c.pool.pick(nil)
	if e == nil {
		t.Fatal("pick() returned nil for a live credential")
	}
	c.pool.markFailureWith(e, kindAuth, "401 unauthorized", 0)

	if got := c.pool.usable(); len(got) != 0 {
		t.Fatalf("usable() = %d entries after an auth error, want 0", len(got))
	}
	st := c.Status(context.Background())
	if got := st.Accounts[0].State; got != stateCooling {
		t.Fatalf("state = %q after an auth error, want %q", got, stateCooling)
	}
}

func TestAccountManagerCRUD(t *testing.T) {
	dir := t.TempDir()
	c := newTestClient(t, dir, `{}`, nil)

	if _, err := c.AddAccount(context.Background(), core.AccountSpec{}); err == nil {
		t.Fatal("an empty access_token must be rejected")
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"access_token": "has space"},
	}); err == nil {
		t.Fatal("an access_token with whitespace must be rejected")
	}
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Label:  "pasted",
		Fields: map[string]string{"access_token": "tok-a", "user_id": "u-a", "refresh_token": "rt-a"},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec.ID != "uid:u-a" || rec.Label != "pasted" || !rec.Enabled {
		t.Fatalf("record = %#v", rec)
	}
	if rec.Identity != "u-a" {
		t.Errorf("Identity = %q", rec.Identity)
	}

	recs, err := c.Accounts(context.Background())
	if err != nil || len(recs) != 1 {
		t.Fatalf("Accounts = %#v, %v", recs, err)
	}
	if got := c.AccountFields(context.Background()); len(got) == 0 {
		t.Error("AccountFields must describe the paste form")
	} else {
		found := false
		for _, f := range got {
			if f.Key == "access_token" {
				found = f.Required
			}
		}
		if !found {
			t.Error("access_token must be a REQUIRED field")
		}
	}

	if err := c.SetAccountEnabled(context.Background(), "uid:u-a", false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	recs, _ = c.Accounts(context.Background())
	if recs[0].Enabled {
		t.Error("the account must be parked")
	}
	if err := c.SetAccountEnabled(context.Background(), "uid:nope", false); err == nil {
		t.Error("an unknown account id must be an error")
	}
	if err := c.ReviveAccount(context.Background(), "uid:nope"); err == nil {
		t.Error("reviving an unknown account id must be an error")
	}
	if err := c.ReviveAccount(context.Background(), "uid:u-a"); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	recs, _ = c.Accounts(context.Background())
	if !recs[0].Enabled {
		t.Error("a revive must re-enable the account")
	}
	if err := c.RemoveAccount(context.Background(), "uid:u-a"); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if recs, _ := c.Accounts(context.Background()); len(recs) != 0 {
		t.Fatalf("Accounts = %#v, want empty", recs)
	}
}

func TestRemoveAccountRefusesAConfigCredential(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{"access_token":"tok-cfg","user_id":"u-cfg"}`, nil)
	err := c.RemoveAccount(context.Background(), "uid:u-cfg")
	if err == nil {
		t.Fatal("a config-supplied account must not be removable from the panel")
	}
	if !strings.Contains(err.Error(), "config") {
		t.Errorf("err = %v, want it to explain that the config owns the account", err)
	}
}

func TestDiscoverAndImportCredentialFile(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "raccoon.json")
	if err := os.WriteFile(bundle, []byte(`{"access_token":"tok-file","refresh_token":"rt-file","user_id":"u-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "nope.json")
	c := newTestClient(t, dir, fmt.Sprintf(`{"credential_paths":[%q,%q]}`, bundle, missing), nil)

	found, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("Discover = %#v", found)
	}
	if !found[0].Importable || found[0].Imported {
		t.Errorf("discovery[0] = %#v, want importable and not yet imported", found[0])
	}
	if found[0].Label == "" {
		t.Error("a discovered credential must carry a label")
	}
	if found[1].Importable {
		t.Errorf("a missing file must not be importable: %#v", found[1])
	}

	recs, err := c.Import(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "uid:u-file" {
		t.Fatalf("Import = %#v", recs)
	}
	found, _ = c.Discover(context.Background())
	if !found[0].Imported {
		t.Error("a second Discover must report the credential as already imported")
	}

	// The bundle shapes an operator is likely to paste.
	for name, body := range map[string]string{
		"array":    `[{"access_token":"a1"},{"access_token":"a2"},{"refresh_token":"no-token"}]`,
		"wrapped":  `{"credentials":[{"access_token":"b1"}]}`,
		"accounts": `{"accounts":[{"access_token":"c1"}]}`,
	} {
		creds, err := parseCredentialBundle([]byte(body))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(creds) == 0 {
			t.Errorf("%s: no credentials parsed", name)
		}
	}
	for _, bad := range []string{``, `{}`, `[]`, `not json`, `{"access_token":""}`} {
		if _, err := parseCredentialBundle([]byte(bad)); err == nil {
			t.Errorf("parseCredentialBundle(%q) must fail", bad)
		}
	}
}

func TestClassifyErr(t *testing.T) {
	cases := []struct {
		err  error
		want errKind
	}{
		{nil, kindNone},
		{&apiError{Status: 401}, kindAuth},
		{&apiError{Status: 403}, kindAuth},
		{&apiError{Code: refreshCodeDead}, kindAuth},
		{&apiError{Status: 402}, kindQuota},
		{&apiError{Status: 429}, kindTransient},
		{&apiError{Status: 503}, kindTransient},
		{&apiError{Status: 400}, kindClient},
		{fmt.Errorf("wrapped: %w", errSessionDead), kindAuth},
		{context.DeadlineExceeded, kindNetwork},
		{errors.New("dial tcp: connection refused"), kindNetwork},
		{errors.New("something odd"), kindNone},
	}
	for _, c := range cases {
		if got := classifyErr(c.err); got != c.want {
			t.Errorf("classifyErr(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestRegisteredUnderRaccoon(t *testing.T) {
	found := false
	for _, n := range core.Registered() {
		if n == "raccoon" {
			found = true
		}
	}
	if !found {
		t.Fatalf(`core.Register("raccoon", New) did not run; registered: %v`, core.Registered())
	}
	c, err := core.Build("raccoon", core.Deps{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if c.Name() != "raccoon" {
		t.Fatalf("Name = %q", c.Name())
	}
	// A malformed config must never take the module down.
	c2, err := core.Build("raccoon", core.Deps{DataDir: t.TempDir(), Config: json.RawMessage(`{ not json`)})
	if err != nil {
		t.Fatalf("a malformed config must not fail construction: %v", err)
	}
	if st := c2.Status(context.Background()); st.Ready {
		t.Error("a module built from a broken config must report not-ready, not panic")
	}
}

// A per-account ceiling is backpressure, not an account failure. The pool
// must rotate to the next eligible account instead of parking the busy one
// or sending its request upstream.
func TestChatSkipsABusyAccountAndUsesTheNext(t *testing.T) {
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Header.Get("Authorization"))
		writeSSE(w, `{"choices":[{"delta":{"content":"ok"}}]}`, `{"choices":[{"finish_reason":"stop"}]}`, `[DONE]`)
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q,"accounts":[{"access_token":"tok-a","user_id":"u-a"},{"access_token":"tok-b","user_id":"u-b"}]}`, ts.URL), ts.Client())

	attempts := map[string]int{}
	var servedBy string
	req := &core.ChatRequest{
		Model:    "sn-kimi-k3",
		Messages: []core.Message{{Role: "user", Content: "q"}},
		ServedBy: &servedBy,
	}
	req.SetAccountAcquirer(func(accountID string) (func(), error) {
		attempts[accountID]++
		if accountID == "uid:u-a" {
			return nil, core.ErrBusy
		}
		return func() {}, nil
	})

	s, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := drain(t, s)
	if got := textOf(events); got != "ok" {
		t.Fatalf("text = %q, want the second account's response", got)
	}
	if attempts["uid:u-a"] != 1 || attempts["uid:u-b"] != 1 {
		t.Fatalf("account attempts = %#v, want one refused slot check then one successful pick", attempts)
	}
	if len(calls) != 1 || calls[0] != "Bearer tok-b" {
		t.Fatalf("upstream authorizations = %v, want only the second account's token", calls)
	}
	if servedBy != "uid:u-b" {
		t.Fatalf("ServedBy = %q, want uid:u-b", servedBy)
	}
}
