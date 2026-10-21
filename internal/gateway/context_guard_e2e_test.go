package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

// ---------------------------------------------------------------------------
// The bug this guards against: the catalogue advertises a window an order of
// magnitude larger than the vendor's real one, a client packs to the advertised
// figure, and every request is refused with 11115.
// ---------------------------------------------------------------------------

// bunnyClient publishes an inflated 1M window for cn:space-bunny and enforces
// the vendor's real, much smaller window.
type bunnyClient struct {
	name string
	// realWindow is what the vendor actually enforces.
	realWindow int64
	// seen records the last request so a test can assert what was sent.
	seen *core.ChatRequest
}

func (c *bunnyClient) Name() string { return c.name }

func (c *bunnyClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "cn:space-bunny", OwnedBy: c.name, Extra: map[string]any{
		"context_length":    int64(1000000),
		"max_output_tokens": int64(128000),
	}}}, nil
}

func (c *bunnyClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.seen = req
	estimate := int64(estimateRequestTokens(req))
	if estimate > c.realWindow {
		// This is workbuddy's code 11115, verbatim.
		return nil, core.Fail(c.name, "", core.FailureContextWindow, 400,
			fmt.Errorf("upstream prompt_too_long (http 400): {\"code\":11115,\"msg\":\"prompt is too long: %d tokens > %d maximum\"}",
				estimate, c.realWindow))
	}
	return &fakeStream{events: []core.Event{{Type: core.EventDone, Finish: "stop"}}}, nil
}

func (c *bunnyClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

// newGuardServer builds the gateway's own *server so a test can reach the guard
// helpers directly, alongside the HTTP surface.
func newGuardServer(t *testing.T, c core.Client, learnedPath string, opts ...func(*Options)) *server {
	t.Helper()
	o := Options{
		Registry:           core.NewRegistry(),
		Version:            "test",
		Logger:             log.New(io.Discard, "", 0),
		LearnedWindowsPath: learnedPath,
	}
	for _, fn := range opts {
		fn(&o)
	}
	o.Registry.Add(c)
	if o.Stats == nil {
		o.Stats = NewStats()
	}
	if o.Usage == nil {
		o.Usage = NewUsageStore(0)
	}
	if o.SessionPlatforms == nil {
		o.SessionPlatforms = NewSessionPlatforms()
	}
	s := &server{
		opts:             o,
		started:          time.Now(),
		stats:            o.Stats,
		sessionPlatforms: o.SessionPlatforms,
		estimates:        newEstimateCalibrationStore(o.EstimateCalibrationPath, nil),
	}
	if s.opts.Logger == nil {
		s.opts.Logger = log.Default()
	}
	s.learnedWindows = openLearnedWindowStore(o.LearnedWindowsPath, s.opts.Logger.Printf)
	// Mirror NewServer: a test that exercises compaction needs the same signer
	// the real process builds.
	s.compaction = newCompactionSigner(o.CompactionKeyPath, s.opts.Logger.Printf)
	s.handler = s.routes()
	return s
}

// oversizedBody builds a conversation of the given shape as a wire request.
func oversizedBody(turns, sizePerTurn int) string {
	msgs := make([]map[string]string, 0, turns)
	for i := 0; i < turns; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]string{
			"role":    role,
			"content": strings.Repeat("a", sizePerTurn),
		})
	}
	b, _ := json.Marshal(map[string]any{
		"model":    "bunny/cn:space-bunny",
		"messages": msgs,
		"stream":   false,
	})
	return string(b)
}

func TestContextGuardServesAnOversizedRequestInsteadOfRefusingIt(t *testing.T) {
	// The vendor's real window is 100k while the catalogue claims 1M.  A request
	// sized over the real window is guaranteed to be refused upstream, so the
	// guard must shorten it first and serve it.
	c := &bunnyClient{name: "bunny", realWindow: 100000}
	s := newGuardServer(t, c, "")

	body := oversizedBody(60, 4000)
	rr := chatHandler(t, s, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected the guard to trim and serve the request, got %d: %s", rr.Code, rr.Body.String())
	}
	if c.seen == nil {
		t.Fatal("the module was never called")
	}
	if int64(estimateRequestTokens(c.seen)) > 100000 {
		t.Fatalf("the trimmed request is still over the real window: %d", estimateRequestTokens(c.seen))
	}
}

func TestTrimmedRequestIsRecordedOnTheUsageRow(t *testing.T) {
	// A trimmed request must be visible as trimmed: silently dropping the
	// caller's own history is not something the gateway may do quietly.
	//
	// The window has to be known up front for a trim to happen, so this seeds the
	// learned limit the refusal path would have recorded.  That is the real
	// sequence: the first request is refused and teaches the gateway, and every
	// request after it is trimmed instead of refused.
	c := &bunnyClient{name: "bunny", realWindow: 100000}
	usage := NewUsageStore(0)
	s := newGuardServer(t, c, "", func(o *Options) { o.Usage = usage })
	s.learnedWindows.put("bunny", "cn:space-bunny", 100000)

	rr := chatHandler(t, s, oversizedBody(120, 4000))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rows := usage.Snapshot()
	var trimmed *UsageRecord
	for i := range rows {
		if rows[i].Trimmed {
			trimmed = &rows[i]
			break
		}
	}
	if trimmed == nil {
		t.Fatalf("no usage row was marked trimmed; rows=%d", len(rows))
	}
	if trimmed.TrimDropped <= 0 {
		t.Fatalf("the row must say how much was dropped, got %d", trimmed.TrimDropped)
	}
	if trimmed.TrimmedFromTokens <= trimmed.TrimmedToTokens {
		t.Fatalf("the row must show the request getting smaller: %d -> %d",
			trimmed.TrimmedFromTokens, trimmed.TrimmedToTokens)
	}
	if trimmed.ContextWindow <= 0 {
		t.Fatalf("the row must record the window it was budgeted against, got %d", trimmed.ContextWindow)
	}
}

func TestVendorLimitIsLearnedFromTheRefusal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "learned_context_windows.json")
	c := &bunnyClient{name: "bunny", realWindow: 100000}
	s := newGuardServer(t, c, path)

	refusal := core.Fail("bunny", "", core.FailureContextWindow, 400,
		fmt.Errorf("upstream prompt_too_long (http 400): {\"code\":11115,\"msg\":\"prompt is too long: 100001 tokens > 100000 maximum\"}"))
	if got := s.learnContextWindowFromFailure(context.Background(), c, "cn:space-bunny", refusal); got != 100000 {
		t.Fatalf("expected to learn 100000, got %d", got)
	}
	if got := s.learnedWindows.get("bunny", "cn:space-bunny"); got != 100000 {
		t.Fatalf("the learned limit must be recorded, got %d", got)
	}
	reopened := openLearnedWindowStore(path, nil)
	if got := reopened.get("bunny", "cn:space-bunny"); got != 100000 {
		t.Fatalf("the learned limit must survive a restart, got %d", got)
	}
}

func TestLearnedWindowOutranksTheInflatedCatalogueInModels(t *testing.T) {
	// This is the part that actually prevents a recurrence: a client that sizes
	// its packing from /v1/models must see the corrected window.
	c := &bunnyClient{name: "bunny", realWindow: 100000}
	s := newGuardServer(t, c, filepath.Join(t.TempDir(), "learned.json"))
	if !s.learnedWindows.put("bunny", "cn:space-bunny", 100000) {
		t.Fatal("seeding the learned window must report a change")
	}

	rr := httptest.NewRecorder()
	s.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "1000000") {
		t.Fatalf("/v1/models must not keep advertising the inflated 1000000, got %s", clip(body))
	}
	if !strings.Contains(body, "100000") {
		t.Fatalf("/v1/models must advertise the learned window, got %s", clip(body))
	}
}

func TestNoLearningWithoutAVendorNumber(t *testing.T) {
	// A refusal that carries no number must change nothing: inventing a limit
	// from a vague sentence is worse than leaving the catalogue alone.
	c := &bunnyClient{name: "bunny", realWindow: 100000}
	s := newGuardServer(t, c, "")
	refusal := core.Fail("bunny", "", core.FailureContextWindow, 400,
		fmt.Errorf("request context exceeds the model's limit; reduce history/message size"))
	if got := s.learnContextWindowFromFailure(context.Background(), c, "cn:space-bunny", refusal); got != 0 {
		t.Fatalf("nothing should be learned from a vague refusal, got %d", got)
	}
	if got := s.learnedWindows.get("bunny", "cn:space-bunny"); got != 0 {
		t.Fatalf("a vague refusal must not record a limit, got %d", got)
	}
}

func TestOperatorOverrideOutranksALearnedLimit(t *testing.T) {
	// An operator's manual number is the final authority.
	dir := t.TempDir()
	store, err := modelmeta.OpenOverrideStore(filepath.Join(dir, "model_context.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("bunny", "cn:space-bunny", 250000, 0); err != nil {
		t.Fatal(err)
	}
	c := &bunnyClient{name: "bunny", realWindow: 100000}
	s := newGuardServer(t, c, filepath.Join(dir, "learned.json"), func(o *Options) {
		o.ModelOverrides = store
	})

	refusal := core.Fail("bunny", "", core.FailureContextWindow, 400,
		fmt.Errorf("prompt is too long: 100001 tokens > 100000 maximum"))
	s.learnContextWindowFromFailure(context.Background(), c, "cn:space-bunny", refusal)

	if got, _ := store.Get("bunny", "cn:space-bunny"); got.ContextLength != 250000 {
		t.Fatalf("the operator's override must be untouched, got %d", got.ContextLength)
	}
	// The operator's number is what the request is judged against.
	if got := s.effectiveContextWindow(context.Background(), c, "cn:space-bunny"); got != 250000 {
		t.Fatalf("the operator's window must win, got %d", got)
	}
}

func clip(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

// chatHandler posts a chat request through the gateway's own handler.
func chatHandler(t *testing.T, s *server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.handler.ServeHTTP(rr, req)
	return rr
}
