package tabbit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

const (
	webTestUID  = "uid-web-0001"
	webTestRoom = "room-web-1"
)

// fakeWeb stands in for web.tabbit.com.  It answers the five endpoints the web
// transport calls and remembers what it was asked, so a test can prove the
// call order (session, join, runs), the headers and the request body.
type fakeWeb struct {
	*httptest.Server

	mu      sync.Mutex
	order   []string
	runBody map[string]any
	runHead http.Header
}

func newFakeWeb(t *testing.T) *fakeWeb {
	t.Helper()
	f := &fakeWeb{}
	mux := http.NewServeMux()

	needCookie := func(w http.ResponseWriter, r *http.Request) bool {
		if c := r.Header.Get("Cookie"); !strings.HasPrefix(c, "token=") {
			t.Errorf("the call to %s carried no token cookie (Cookie=%q)", r.URL.Path, c)
			http.Error(w, "no cookie", http.StatusUnauthorized)
			return false
		}
		return true
	}

	mux.HandleFunc("/panel/session", func(w http.ResponseWriter, r *http.Request) {
		f.note("session")
		if !needCookie(w, r) {
			return
		}
		writeWebJSON(w, `{"chat_session_id":"`+webTestRoom+`"}`)
	})

	mux.HandleFunc(webModelsPath, func(w http.ResponseWriter, r *http.Request) {
		f.note("models")
		if !needCookie(w, r) {
			return
		}
		writeWebJSON(w, `{"models":[`+
			`{"display_name":"Default","supports_tools":true,"support_thinking":true,`+
			`"model_access_type":"free_unlimited","display_multiplier":0.0},`+
			`{"display_name":"GLM-5.3","model_access_type":"free_metered","display_multiplier":1.0}],`+
			`"status":"success"}`)
	})

	mux.HandleFunc(webUsagePath, func(w http.ResponseWriter, r *http.Request) {
		f.note("usage")
		if !needCookie(w, r) {
			return
		}
		if got := r.URL.Query().Get("user_id"); got != webTestUID {
			t.Errorf("the quota was asked for user_id=%q, want %q", got, webTestUID)
		}
		writeWebJSON(w, `{"member_level":"pro","usage_percentage":12.5,"remaining_reset_hours":5,`+
			`"current_cycle_start":"2026-09-01T00:00:00Z","current_cycle_end":"2026-10-01T00:00:00Z",`+
			`"is_subscription":true}`)
	})

	mux.HandleFunc("/api/v3/chat/rooms/"+webTestRoom+"/runs", func(w http.ResponseWriter, r *http.Request) {
		f.note("runs")
		if !needCookie(w, r) {
			return
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("the run body is not JSON: %v (%s)", err, raw)
		}
		f.mu.Lock()
		f.runBody, f.runHead = body, r.Header.Clone()
		f.mu.Unlock()
		writeWebJSON(w, `{"run_id":"run-web-1","room_id":"`+webTestRoom+`","status":"QUEUED"}`)
	})

	mux.HandleFunc("/api/v3/chat/rooms/"+webTestRoom+"/join", func(w http.ResponseWriter, r *http.Request) {
		f.note("join")
		if !needCookie(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		emit := func(typ, payload string) {
			io.WriteString(w, `data: {"event_id":"evt-1","event_type":"`+typ+`","payload":`+payload+"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		// The vendor replays the room's history before our run starts.  Those
		// deltas must be dropped, and an event this module has never seen must
		// not end the stream.
		emit("snapshot_begin", `{"last_event_id":null}`)
		emit("assistant_message_delta", `{"delta":"SNAPSHOT","chunk_type":"message_chunk"}`)
		emit("message_history", `{"messages":[]}`)
		emit("active_runs_snapshot", `{"runs":[]}`)
		emit("snapshot_end", `{}`)
		emit("run_queued", `{"run_id":"run-web-1","status":"QUEUED"}`)
		emit("run_started", `{"run_id":"run-web-1","status":"RUNNING"}`)
		// Reasoning arrives on its own event type, in chunks, interleaved with
		// the answer.  Both channels must survive.
		emit("event_message_chunk", `{"chunk_type":"thinking","chunk_payload":{"reasoning_content":"think "}}`)
		emit("event_message_chunk", `{"chunk_type":"thinking","chunk_payload":{"reasoning_content":"hard"}}`)
		emit("event_message_chunk", `{"chunk_type":"thinking_finished","chunk_payload":{"message_id":"m1"}}`)
		emit("assistant_message_delta", `{"delta":"WEB","chunk_type":"message_chunk"}`)
		emit("session_title_updated", `{"title":"hi"}`)
		emit("assistant_message_delta", `{"delta":"OK","chunk_payload":{"content":"OK"}}`)
		// A merged frame carries both channels at once; the text must not be
		// lost just because reasoning was also present.
		emit("assistant_message_delta", `{"delta":"!","chunk_type":"message_chunk","chunk_payload":{"reasoning_content":"tail"}}`)
		emit("assistant_message_finished", `{"chunk_type":"message_finish"}`)
		emit("run_completed", `{"status":"COMPLETED"}`)
		emit("room_reset_required", `{"status":"COMPLETED","reason":"terminal_snapshot_available"}`)
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

func writeWebJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, body)
}

func (f *fakeWeb) note(step string) {
	f.mu.Lock()
	f.order = append(f.order, step)
	f.mu.Unlock()
}

func (f *fakeWeb) steps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func (f *fakeWeb) hits(step string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.order {
		if s == step {
			n++
		}
	}
	return n
}

func (f *fakeWeb) body() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runBody
}

func (f *fakeWeb) header() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runHead
}

// newWebClient builds a module whose web transport points at the fake vendor,
// with one web-token account already stored.
func newWebClient(t *testing.T, f *fakeWeb) (*Client, core.AccountRecord) {
	t.Helper()
	return newWebClientHTTP(t,
		`{"transport":"web","web_base_url":"`+f.URL+`"}`, f.Client())
}

// slowGateTransport holds one vendor path back on demand, so a test can make a
// probe run past its budget without touching any other call.
type slowGateTransport struct {
	base  http.RoundTripper
	path  string
	delay atomic.Int64 // nanoseconds; 0 means "pass through"
}

func (s *slowGateTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == s.path {
		if d := time.Duration(s.delay.Load()); d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
	}
	base := s.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// newWebClientHTTP is newWebClient with the module config and the HTTP client
// left to the caller, so a test can shorten a budget or slow one endpoint down.
func newWebClientHTTP(t *testing.T, cfgJSON string, hc *http.Client) (*Client, core.AccountRecord) {
	t.Helper()
	clearTabbitEnv(t)
	c := newTestClient(t, cfgJSON, hc)
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{
			"kind":  "web-token",
			"token": webJWTFor(t, webTestUID, time.Now().Add(time.Hour)),
		},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	return c, rec
}

// TestTabbitWebChatStreamsThroughTheVendorProtocol walks the whole request path
// the module claims to implement: create a room, open the join stream, submit
// the run, take the live deltas until the run completes, and throw the room
// away.
func TestTabbitWebChatStreamsThroughTheVendorProtocol(t *testing.T) {
	f := newFakeWeb(t)
	c, rec := newWebClient(t, f)
	ctx := context.Background()

	if want := webAccountIDTag + webTestUID; rec.ID != want {
		t.Fatalf("account id = %q, want %q", rec.ID, want)
	}
	if !c.webRoute() {
		t.Fatal("a web-token account did not put the module on the web transport")
	}

	stream, err := c.Chat(ctx, &core.ChatRequest{
		Model:    "Default",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, reasoning, err := drainTabbitChannels(stream)
	if err != nil {
		t.Fatalf("drain the stream: %v", err)
	}
	if text != "WEBOK!" {
		t.Fatalf("reply = %q, want %q (the snapshot delta must be dropped and every live delta kept exactly once)",
			text, "WEBOK!")
	}
	if want := "think hardtail"; reasoning != want {
		t.Errorf("reasoning = %q, want %q (the thinking channel is its own event type and must not be dropped)",
			reasoning, want)
	}

	got := f.steps()
	want := []string{"session", "join", "runs"}
	if len(got) != len(want) {
		t.Fatalf("web calls = %v, want %v (create the room, join it, then submit the run)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("web calls = %v, want %v (create the room, join it, then submit the run)", got, want)
		}
	}

	body := f.body()
	payload, _ := body["input_payload"].(map[string]any)
	if payload == nil {
		t.Fatalf("the run body has no input_payload: %#v", body)
	}
	if content, _ := payload["content"].(string); content != "hi" {
		t.Errorf("input_payload.content = %q, want %q", content, "hi")
	}
	models, _ := payload["selected_model"].([]any)
	if len(models) != 1 || models[0] != "Default" {
		t.Errorf("selected_model = %#v, want [Default] (the vendor keys models by display name)",
			payload["selected_model"])
	}
	meta, _ := payload["metadatas"].(map[string]any)
	if html, _ := meta["html_content"].(string); html != "<p>hi</p>" {
		t.Errorf("metadatas.html_content = %q, want %q", html, "<p>hi</p>")
	}
	if h := f.header(); h.Get("X-Req-Ctx") == "" {
		t.Error("the run request carried no x-req-ctx header")
	}
}

// The web transport's submit endpoint takes one content string, so an image
// part has nowhere to go.  Dropping it silently is the worst outcome available:
// the model answers as if it had been asked about something it never saw, and
// the caller cannot tell the answer was fabricated.  Observed live against
// web.tabbit.com -- it replied "I don't see any image attached".  The request
// must be refused instead, and refused before any vendor call is made.
func TestTabbitWebChatRefusesAnImageInsteadOfDroppingIt(t *testing.T) {
	f := newFakeWeb(t)
	c, _ := newWebClient(t, f)
	ctx := context.Background()

	_, err := c.Chat(ctx, &core.ChatRequest{
		Model: "Default",
		Messages: []core.Message{{
			Role: "user",
			Parts: []core.ContentPart{
				{Type: "text", Text: "what colour is the top-left corner?"},
				{Type: "image_url", ImageURL: "data:image/png;base64,iVBORw0KGgo="},
			},
		}},
	})
	if err == nil {
		t.Fatal("an image part was accepted by a transport that cannot carry it")
	}
	if !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("error = %v, want one wrapping core.ErrUnsupported so the gateway can type it", err)
	}
	if !strings.Contains(err.Error(), "image") {
		t.Errorf("error = %v, want it to name the image so the operator knows what to change", err)
	}
	if calls := f.steps(); len(calls) != 0 {
		t.Errorf("vendor calls = %v, want none: the request must fail before the room is opened", calls)
	}

	// A text-only request on the same client must still work, so the guard is
	// not "reject anything with Parts".
	stream, err := c.Chat(ctx, &core.ChatRequest{
		Model:    "Default",
		Messages: []core.Message{{Role: "user", Parts: []core.ContentPart{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("a text-only part array must still be accepted: %v", err)
	}
	reply, err := drainTabbitStream(stream)
	if err != nil {
		t.Fatalf("drain the stream: %v", err)
	}
	if reply != "WEBOK!" {
		t.Errorf("reply = %q, want WEBOK!", reply)
	}
}

// TestTabbitWebStatusReusesAFreshVerdict proves the panel's poll loop does not
// spend the status budget on the catalogue again right after a chat stored a
// verdict of its own.
func TestTabbitWebStatusReusesAFreshVerdict(t *testing.T) {
	f := newFakeWeb(t)
	c, rec := newWebClient(t, f)
	ctx := context.Background()

	first := c.Status(ctx)
	if n := f.hits("models"); n != 1 {
		t.Fatalf("the first Status asked the vendor %d times, want 1", n)
	}
	if !first.Ready {
		t.Fatalf("a working cookie did not make the module ready: %+v", first)
	}

	c.storeWebVerdict(rec.ID, webVerdict{at: time.Now(), models: 2})
	second := c.Status(ctx)
	if n := f.hits("models"); n != 1 {
		t.Fatalf("Status asked the vendor %d times, want 1: a verdict younger than %s must be reused",
			n, webVerdictTTL)
	}
	if len(second.Accounts) != 1 {
		t.Fatalf("Status listed %d accounts, want 1", len(second.Accounts))
	}
	if second.Accounts[0].State != epStateReady {
		t.Errorf("state = %q, want %q", second.Accounts[0].State, epStateReady)
	}
	if !strings.Contains(second.Accounts[0].Note, "2 models from web.tabbit.com") {
		t.Errorf("the reused verdict is not reported in the note: %q", second.Accounts[0].Note)
	}
}

// TestTabbitWebStatusKeepsTheLastGoodVerdictWhenTheProbeRunsOutOfBudget pins
// the fix for the account-pool badge that flickered red.  The chip is drawn
// from Status() while the row under it is drawn from Accounts(), so a status
// probe that runs past its budget must not answer with a state the account row
// does not share.
func TestTabbitWebStatusKeepsTheLastGoodVerdictWhenTheProbeRunsOutOfBudget(t *testing.T) {
	f := newFakeWeb(t)
	gate := &slowGateTransport{base: f.Client().Transport, path: webModelsPath}
	// 50ms is a budget the local fake meets easily until the gate holds the
	// catalogue back.
	c, rec := newWebClientHTTP(t,
		`{"transport":"web","web_base_url":"`+f.URL+`","timeouts":{"status_seconds":0.05}}`,
		&http.Client{Transport: gate})
	ctx := context.Background()

	first := c.Status(ctx)
	if !first.Ready {
		t.Fatalf("the first, fast probe did not verify the cookie: %+v", first)
	}

	gate.delay.Store(int64(300 * time.Millisecond))
	// Age the verdict past webVerdictTTL so Status actually spends the budget
	// on the catalogue again, while keeping it inside webTrustWindow so it is
	// still the last real answer.
	c.storeWebVerdict(rec.ID, webVerdict{at: time.Now().Add(-webVerdictTTL - time.Second), models: 2})
	second := c.Status(ctx)
	if !second.Ready {
		t.Fatalf("a timed-out probe dropped a cookie that was verified moments ago: %+v", second.Accounts)
	}
	if got := second.Accounts[0].State; got != epStateReady {
		t.Errorf("state = %q, want %q", got, epStateReady)
	}
	if !strings.Contains(second.Accounts[0].Note, "ran out of budget") {
		t.Errorf("the note does not say the probe ran out of budget: %q", second.Accounts[0].Note)
	}

	// The row the operator clicks into reads its state from Accounts().  A row
	// that says ready under a badge that says the opposite is the bug.
	list, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(list) != 1 || list[0].ID != rec.ID {
		t.Fatalf("Accounts = %+v, want one row for %s", list, rec.ID)
	}
	if list[0].State != second.Accounts[0].State {
		t.Errorf("the account row says %q while Status says %q; the two views must agree",
			list[0].State, second.Accounts[0].State)
	}
}

// TestTabbitWebStatusDoesNotTrustAVerdictForever bounds that fallback: a check
// that succeeded longer ago than webTrustWindow is no longer evidence, so an
// unreachable vendor leaves the credential unknown rather than claiming it works.
func TestTabbitWebStatusDoesNotTrustAVerdictForever(t *testing.T) {
	f := newFakeWeb(t)
	gate := &slowGateTransport{base: f.Client().Transport, path: webModelsPath}
	gate.delay.Store(int64(300 * time.Millisecond))
	c, rec := newWebClientHTTP(t,
		`{"transport":"web","web_base_url":"`+f.URL+`","timeouts":{"status_seconds":0.05}}`,
		&http.Client{Transport: gate})
	ctx := context.Background()

	c.storeWebVerdict(rec.ID, webVerdict{at: time.Now().Add(-2 * webTrustWindow), models: 2})

	st := c.Status(ctx)
	if st.Ready {
		t.Fatalf("a verdict older than %s still made the module ready: %+v", webTrustWindow, st.Accounts)
	}
	if got := st.Accounts[0].State; got != epStateUnknown {
		t.Errorf("Status state = %q, want %q", got, epStateUnknown)
	}

	list, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if got := list[0].State; got != epStateUnknown {
		t.Errorf("the account row says %q, want %q", got, epStateUnknown)
	}
}

// TestTabbitWebTestAccountReportsTheQuota covers the other real thing the
// cookie buys: the vendor's own quota report, folded into the test result
// because the module deliberately refuses to invent a credit count.
func TestTabbitWebTestAccountReportsTheQuota(t *testing.T) {
	f := newFakeWeb(t)
	c, rec := newWebClient(t, f)

	res, err := c.TestAccount(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount failed: %+v", res)
	}
	if !strings.Contains(res.Reply, "WEBOK") {
		t.Errorf("the test result does not quote the completion: %q", res.Reply)
	}
	if !strings.Contains(res.Reply, "member pro") || !strings.Contains(res.Reply, "12.5% of the quota used") {
		t.Errorf("the quota report never reached the test result: %q", res.Reply)
	}
	if n := f.hits("usage"); n != 1 {
		t.Errorf("the quota endpoint was called %d times, want 1", n)
	}
}

// TestTabbitLauncherHintNamesTheWayOut pins the one launcher failure the
// operator has to resolve by hand.  A launcher that refuses to come up under
// the service process refuses on every retry, so the message must name the
// manual route instead of inviting a second press of the same button.
func TestTabbitLauncherHintNamesTheWayOut(t *testing.T) {
	plain := "the Tabbit browser returned an empty result"
	if got := launcherHint(plain); got != plain {
		t.Errorf("an unrelated failure was rewritten: %q", got)
	}

	raw := `{"error":{"code":"BROWSER_LAUNCH_FAILED","message":"Cannot launch Tabbit Browser"}}`
	got := launcherHint(raw)
	if !strings.Contains(got, raw) {
		t.Errorf("the launcher's own words were dropped: %q", got)
	}
	for _, want := range []string{cliTaskName, "token", "web.tabbit.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("the hint does not mention %q: %q", want, got)
		}
	}
}

// TestTabbitWebChatNamesTheServedAccount pins the gateway-facing attribution for
// the web transport.  The session id the account table reports is
// tabbit-web:<uid>, and openWebStream settles it before the first network call.
// Before the ServedBy slot existed every success was filed under "(unrouted)".
func TestTabbitWebChatNamesTheServedAccount(t *testing.T) {
	f := newFakeWeb(t)
	c, rec := newWebClient(t, f)

	want := webAccountIDTag + webTestUID
	if rec.ID != want {
		t.Fatalf("account id = %q, want %q", rec.ID, want)
	}

	var served string
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "Default",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		ServedBy: &served,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, err := drainTabbitStream(stream); err != nil {
		t.Fatalf("drain the stream: %v", err)
	}

	if served != want {
		t.Errorf("ServedBy = %q, want %q", served, want)
	}
}

// TestTheWebReasoningChannelSurvivesTheSwitch pins the one frame shape that is
// easiest to lose.  The model's thinking does not arrive as an
// assistant_message_delta: it arrives as event_message_chunk with
// chunk_type=thinking, on its own event type.  A webStream that only knew the
// delta event would route every one of those frames into `default:` and drop
// them -- and a live run on 2026-10-02 carried 103 thinking frames against 28
// answer frames, so "most of the model's output" is not an exaggeration.
//
// The frames below are the shapes the vendor actually sent, trimmed to the
// fields this module reads.
func TestTheWebReasoningChannelSurvivesTheSwitch(t *testing.T) {
	cases := []struct {
		name   string
		frame  sseFrame
		text   string
		reason string
		emits  bool
	}{
		{
			name:   "a thinking chunk becomes reasoning and never text",
			frame:  sseFrame{Data: `{"event_type":"event_message_chunk","payload":{"chunk_type":"thinking","chunk_payload":{"reasoning_content":"weigh the options"}}}`},
			reason: "weigh the options",
			emits:  true,
		},
		{
			name:   "a top-level reasoning_content on a thinking chunk is read too",
			frame:  sseFrame{Data: `{"event_type":"event_message_chunk","payload":{"chunk_type":"thinking","reasoning_content":"second guess"}}`},
			reason: "second guess",
			emits:  true,
		},
		{
			name:  "the far end of the reasoning channel emits nothing",
			frame: sseFrame{Data: `{"event_type":"event_message_chunk","payload":{"chunk_type":"thinking_finished","chunk_payload":{"message_id":"m1"}}}`},
		},
		{
			name:  "the answer still arrives as a delta",
			frame: sseFrame{Data: `{"event_type":"assistant_message_delta","payload":{"delta":"the answer","chunk_type":"message_chunk"}}`},
			text:  "the answer",
			emits: true,
		},
		{
			name:   "a merged delta carries both channels at once",
			frame:  sseFrame{Data: `{"event_type":"assistant_message_delta","payload":{"delta":"tail","chunk_payload":{"reasoning_content":"and more"}}}`},
			text:   "tail",
			reason: "and more",
			emits:  true,
		},
		{
			name:  "an event this module has never seen is ignored, not fatal",
			frame: sseFrame{Data: `{"event_type":"session_title_updated","payload":{"chunk_type":"title"}}`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newWebStream(io.NopCloser(strings.NewReader("")), nil, nil)
			s.started = true // run_started has been seen; the replay guard is off

			events, terminal := s.consume(tc.frame)
			if terminal {
				t.Fatalf("a non-terminal frame ended the stream")
			}
			if !tc.emits {
				if len(events) != 0 {
					t.Fatalf("the frame emitted %d events, want none: %+v", len(events), events)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("the frame emitted %d events, want exactly one: %+v", len(events), events)
			}
			if events[0].Type != core.EventDelta {
				t.Fatalf("event type = %q, want %q", events[0].Type, core.EventDelta)
			}
			if events[0].Delta != tc.text {
				t.Errorf("text = %q, want %q", events[0].Delta, tc.text)
			}
			if events[0].Reasoning != tc.reason {
				t.Errorf("reasoning = %q, want %q", events[0].Reasoning, tc.reason)
			}
		})
	}

	// The other half of the contract: the room's replayed history must still be
	// dropped.  Reasoning that arrives before the run starts belongs to an
	// earlier turn, and forwarding it would prepend a stale answer's thinking to
	// this one.
	t.Run("reasoning replayed before the run started is still dropped", func(t *testing.T) {
		s := newWebStream(io.NopCloser(strings.NewReader("")), nil, nil)
		frame := sseFrame{Data: `{"event_type":"event_message_chunk","payload":{"chunk_type":"thinking","chunk_payload":{"reasoning_content":"an earlier turn"}}}`}
		if events, _ := s.consume(frame); len(events) != 0 {
			t.Fatalf("a frame from the replay emitted %d events: %+v", len(events), events)
		}
	})
}
