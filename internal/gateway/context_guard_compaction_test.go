package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

// driveResponses runs the Responses endpoint against the guard server's own
// handler, so a test can reach both the guard and the wire shape.
func driveResponses(t *testing.T, s *server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

// answeringCatalogueClient is a catalogueClient whose stream produces one
// visible token, so an ordinary turn completes instead of being reported as an
// empty completion.
type answeringCatalogueClient struct {
	catalogueClient
	seen *core.ChatRequest
}

func (c *answeringCatalogueClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.seen = req
	return &fakeStream{events: []core.Event{
		{Type: core.EventDelta, Delta: "ok"},
		{Type: core.EventDone, Finish: "stop"},
	}}, nil
}

// bigResponsesHistory builds a conversation the guard must trim against the
// small window these tests use, ending in the question that has to survive
// verbatim.
func bigResponsesHistory(turns int) string {
	items := make([]string, 0, turns+1)
	for i := 0; i < turns; i++ {
		items = append(items, `{"type":"message","role":"user","content":"`+strings.Repeat("a", 2000)+`"}`)
	}
	items = append(items, `{"type":"message","role":"user","content":"the current question"}`)
	return `{"model":"autoclaw/cn:space-bunny","stream":false,"input":[` + strings.Join(items, ",") + `]}`
}

// compactionItemsIn returns every output item of type "compaction".
func compactionItemsIn(t *testing.T, rr *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("not JSON: %v: %s", err, rr.Body.String())
	}
	var out []map[string]any
	items, _ := env["output"].([]any)
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && m["type"] == "compaction" {
			out = append(out, m)
		}
	}
	return out
}

// The bug this closes: the guard trims the caller's history to fit, the turn
// succeeds, and the caller is told nothing -- so the next turn re-sends the same
// oversized history and gets trimmed again, forever.  Responses has a channel
// for the answer: a compaction item in the turn's output, which the client
// carries forward as folded history.
func TestTrimmedResponsesTurnCarriesACompactionItem(t *testing.T) {
	c := &answeringCatalogueClient{catalogueClient: catalogueClient{name: "autoclaw", window: 5000}}
	s := newGuardServer(t, c, "", func(o *Options) {
		o.CompactionKeyPath = filepath.Join(t.TempDir(), "compaction.key")
	})

	body := bigResponsesHistory(8)

	// Precondition, stated through the production predicate: this history is
	// one the guard really would have trimmed.  If that ever stops being true,
	// this test must fail here rather than silently asserting nothing.
	pre, err := toCoreRequestFromResponses(mustResponsesRequest(t, body), s.compaction)
	if err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if !s.responsesWouldTrim(context.Background(), pre) {
		t.Fatal("precondition: the history must be one the guard would trim")
	}

	rr := driveResponses(t, s, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	items := compactionItemsIn(t, rr)
	if len(items) != 1 {
		t.Fatalf("want exactly one compaction item on a trimmed turn, got %d: %s",
			len(items), rr.Body.String())
	}
	blob, _ := items[0]["encrypted_content"].(string)
	if !strings.HasPrefix(blob, "c2c1.") {
		t.Fatalf("compaction item carries no gateway blob: %q", blob)
	}
	// The question is what the vendor must still be asked: the fold replaces
	// stale history with a summary but never the newest user turn.
	if c.seen == nil {
		t.Fatal("the model was never called")
	}
	last := ""
	for _, m := range c.seen.Messages {
		if strings.Contains(m.Content, "the current question") {
			last = m.Content
		}
	}
	if last == "" {
		t.Fatalf("the newest question was folded away: %+v", c.seen.Messages)
	}
	if len(c.seen.Messages) >= 9 {
		t.Fatalf("the fold must shorten the history, still %d messages", len(c.seen.Messages))
	}
}

// A turn that fits is not trimmed, so it must NOT carry a compaction item: the
// caller's own history is still intact and folding it would cost a
// summarisation call for nothing.
func TestUntrimmedResponsesTurnCarriesNoCompactionItem(t *testing.T) {
	c := &answeringCatalogueClient{catalogueClient: catalogueClient{name: "autoclaw", window: 1048576}}
	s := newGuardServer(t, c, "", func(o *Options) {
		o.CompactionKeyPath = filepath.Join(t.TempDir(), "compaction.key")
	})

	rr := driveResponses(t, s, bigResponsesHistory(8))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if items := compactionItemsIn(t, rr); len(items) != 0 {
		t.Fatalf("an untrimmed turn must not carry a compaction item: %s", rr.Body.String())
	}
}

// mustResponsesRequest decodes a wire body the way the endpoint does.
func mustResponsesRequest(t *testing.T, body string) *responsesRequest {
	t.Helper()
	var w responsesRequest
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &w
}
