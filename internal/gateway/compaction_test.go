package gateway

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

// newCompactServer builds a gateway whose compaction key lives in a temp file,
// so tests exercise the same persisted-key path a deployment uses.
func newCompactServer(t *testing.T, c *testClient) *http.Server {
	t.Helper()
	return newCompactServerKeyed(t, c, filepath.Join(t.TempDir(), "compaction.key"))
}

// newCompactServerKeyed builds a gateway with an explicit key path, so a test can
// give two servers the same persisted key -- which is what a real deployment has
// before and after a restart.
func newCompactServerKeyed(t *testing.T, c *testClient, keyPath string) *http.Server {
	t.Helper()
	return NewServer(Options{
		Registry:          regWith(c),
		Version:           "test",
		Logger:            log.New(io.Discard, "", 0),
		Stats:             NewStats(),
		Usage:             NewUsageStore(0),
		CompactionKeyPath: keyPath,
	})
}

// responsesCompact drives POST /v1/responses/compact.
func responsesCompact(t *testing.T, srv *http.Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

// summaryEvents is what the stub model returns for a summarisation call.
func summaryEvents(text string) []core.Event {
	return []core.Event{
		{Type: core.EventDelta, Delta: text},
		{Type: core.EventUsage, Usage: &core.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}},
		{Type: core.EventDone, Finish: "stop"},
	}
}

// decodeCompaction parses a response.compaction envelope.
func decodeCompaction(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, body)
	}
	return m
}

// compactionItems returns every output item whose type is "compaction".
func compactionItems(t *testing.T, envelope map[string]any) []map[string]any {
	t.Helper()
	raw, _ := envelope["output"].([]any)
	var out []map[string]any
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "compaction" {
			out = append(out, m)
		}
	}
	return out
}

func TestResponsesCompactReturnsOneCompactionItemWithABlob(t *testing.T) {
	c := &testClient{name: "stub", events: summaryEvents("GOAL: fix the login bug. NEXT: add a test.")}
	srv := newCompactServer(t, c)

	rr := responsesCompact(t, srv, `{
		"model": "m1",
		"input": [
			{"type":"message","role":"user","content":"fix the login bug"},
			{"type":"message","role":"assistant","content":"I changed login.go and it works"},
			{"type":"message","role":"user","content":"also add a test"}
		]
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	env := decodeCompaction(t, rr.Body.String())
	if env["object"] != "response.compaction" {
		t.Fatalf("object = %v, want response.compaction", env["object"])
	}
	items := compactionItems(t, env)
	if len(items) != 1 {
		t.Fatalf("compaction items = %d, want exactly 1: %s", len(items), rr.Body.String())
	}
	blob, _ := items[0]["encrypted_content"].(string)
	if strings.TrimSpace(blob) == "" {
		t.Fatal("the compaction item carries no blob")
	}
	if !strings.HasPrefix(blob, "c2c1.") {
		t.Fatalf("blob = %q, want the gateway's own version prefix", blob)
	}
	if c.chatCalls == 0 {
		t.Fatal("the summariser was never called")
	}
	// The summary must actually describe the history, not be a placeholder.
	if !strings.Contains(rr.Body.String(), "fix the login bug") {
		t.Fatalf("the summary lost the user's goal: %s", rr.Body.String())
	}
}

func TestResponsesCompactRejectsAnEmptyInput(t *testing.T) {
	c := &testClient{name: "stub", events: summaryEvents("summary")}
	srv := newCompactServer(t, c)

	for _, body := range []string{`{"model":"m1","input":""}`, `{"model":"m1"}`, `{"model":"m1","input":[{"type":"message","role":"assistant","content":"hi"}]}`} {
		rr := responsesCompact(t, srv, body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400: %s", body, rr.Code, rr.Body.String())
		}
	}
}

// The blob this gateway mints must be readable by this gateway and replaced by
// the turn it stands for.  A foreign ciphertext must be dropped instead, exactly
// as before.
func TestResponsesCompactBlobIsReplayedAndForeignCiphertextIsDropped(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "compaction.key")
	c := &testClient{name: "stub", events: summaryEvents("carried state: login.go is fixed")}
	srv := newCompactServerKeyed(t, c, keyPath)

	rr := responsesCompact(t, srv, `{"model":"m1","input":[
		{"type":"message","role":"user","content":"do the thing"},
		{"type":"message","role":"assistant","content":"did it"},
		{"type":"message","role":"user","content":"now the next step"}
	]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("compact status = %d: %s", rr.Code, rr.Body.String())
	}
	env := decodeCompaction(t, rr.Body.String())
	items := compactionItems(t, env)
	if len(items) != 1 {
		t.Fatalf("no compaction item: %s", rr.Body.String())
	}
	blob := items[0]["encrypted_content"].(string)

	// Replaying the blob must bring the summary back as history.
	replay := &testClient{name: "stub", events: summaryEvents("ok")}
	replaySrv := newCompactServerKeyed(t, replay, keyPath)
	body := `{"model":"m1","input":[{"type":"compaction","encrypted_content":` + jsonString(blob) + `},{"type":"message","role":"user","content":"continue"}]}`
	r2 := responses(t, replaySrv, body)
	if r2.Code != http.StatusOK {
		t.Fatalf("replay status = %d: %s", r2.Code, r2.Body.String())
	}
	if replay.seen == nil {
		t.Fatal("the model was never called")
	}
	found := false
	for _, m := range replay.seen.Messages {
		if strings.Contains(m.Content, "carried state") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the replayed blob was not materialised as history: %+v", replay.seen.Messages)
	}

	// A foreign ciphertext must be dropped without failing the request.
	foreign := &testClient{name: "stub", events: summaryEvents("ok")}
	foreignSrv := newCompactServerKeyed(t, foreign, keyPath)
	// A realistic OpenAI blob: a long, space-free base64 run, which is what the
	// legacy heuristic recognises and must keep dropping.
	cipher := "gAAAAABmFAKE" + strings.Repeat("Ab3dEf9Gh2Ij4Kl5Mn6Op7Qr8St9Uv", 20)
	fbody := `{"model":"m1","input":[{"type":"compaction","encrypted_content":` + jsonString(cipher) + `},{"type":"message","role":"user","content":"continue"}]}`
	r3 := responses(t, foreignSrv, fbody)
	if r3.Code != http.StatusOK {
		t.Fatalf("a foreign blob must not fail the turn: %d: %s", r3.Code, r3.Body.String())
	}
	for _, m := range foreign.seen.Messages {
		if strings.Contains(m.Content, "gAAAAABm") {
			t.Fatalf("foreign ciphertext reached the model: %q", m.Content)
		}
	}
}

// context_management must fold the history before routing and append the item.
func TestContextManagementCompactsWhenTheEstimateCrossesTheThreshold(t *testing.T) {
	c := &testClient{name: "stub", events: summaryEvents("summary of the earlier turns")}
	srv := newCompactServer(t, c)

	turns := make([]string, 0, 24)
	for i := 0; i < 24; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		turns = append(turns, `{"type":"message","role":"`+role+`","content":"`+strings.Repeat("x", 400)+`"}`)
	}
	body := `{"model":"m1","stream":false,"context_management":[{"type":"compaction","compact_threshold":100}],"input":[` + strings.Join(turns, ",") + `]}`
	rr := responses(t, srv, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	env := decodeCompaction(t, rr.Body.String())
	items := compactionItems(t, env)
	if len(items) != 1 {
		t.Fatalf("context_management did not append a compaction item: %s", rr.Body.String())
	}
	if strings.TrimSpace(items[0]["encrypted_content"].(string)) == "" {
		t.Fatal("the auto-compaction item has no blob")
	}
}

func TestContextManagementIsIgnoredWithoutAThreshold(t *testing.T) {
	c := &testClient{name: "stub", events: summaryEvents("hello")}
	srv := newCompactServer(t, c)

	body := `{"model":"m1","stream":false,"context_management":[{"type":"compaction"}],"input":[{"type":"message","role":"user","content":"hi"}]}`
	rr := responses(t, srv, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if items := compactionItems(t, decodeCompaction(t, rr.Body.String())); len(items) != 0 {
		t.Fatalf("a threshold-less request must not compact: %s", rr.Body.String())
	}
}

// jsonString renders s as a JSON string literal.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The signing key must round-trip through the file so a blob survives a restart
// of the process.
func TestCompactionKeyPersistsAcrossSigners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compaction.key")
	first := newCompactionSigner(path, nil)
	blob, err := first.mint("carried state", compactionPayload{Model: "m1"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	second := newCompactionSigner(path, nil)
	summary, ok := second.parse(blob)
	if !ok {
		t.Fatal("a blob minted by a previous signer did not verify")
	}
	if summary != "carried state" {
		t.Fatalf("summary = %q", summary)
	}

	// A different key must reject it (tamper / different deployment).
	other := newCompactionSigner(filepath.Join(t.TempDir(), "other.key"), nil)
	if _, ok := other.parse(blob); ok {
		t.Fatal("a blob verified under an unrelated key")
	}
	if _, ok := second.parse(blob + "x"); ok {
		t.Fatal("a tampered blob verified")
	}
}

func TestCompactionSignerRefusesEmptySummaries(t *testing.T) {
	s := newCompactionSigner("", nil)
	if _, err := s.mint("   ", compactionPayload{}); err == nil {
		t.Fatal("mint accepted an empty summary")
	}
	var nilSigner *compactionSigner
	if _, ok := nilSigner.parse("c2c1.abc.def"); ok {
		t.Fatal("a nil signer verified a blob")
	}
}

// A summary that cannot be produced (the model refuses) must still yield a
// usable response.compaction object rather than a 500.
func TestResponsesCompactDegradesWhenSummarisationFails(t *testing.T) {
	c := &testClient{
		name:    "stub",
		chatErr: core.Fail("stub", "", core.FailureUpstream, 502, io.ErrUnexpectedEOF),
	}
	srv := newCompactServer(t, c)

	rr := responsesCompact(t, srv, `{"model":"m1","input":[{"type":"message","role":"user","content":"old question"},{"type":"message","role":"assistant","content":"old answer"},{"type":"message","role":"user","content":"new question"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want a usable 200: %s", rr.Code, rr.Body.String())
	}
	env := decodeCompaction(t, rr.Body.String())
	if env["object"] != "response.compaction" {
		t.Fatalf("object = %v", env["object"])
	}
	if items := compactionItems(t, env); len(items) != 1 {
		t.Fatalf("no compaction item on the degraded path: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "new question") {
		t.Fatalf("the retained turn was dropped: %s", rr.Body.String())
	}
}

// A streamed context_management turn must carry the compaction item on both
// output_item.added and output_item.done, and include it in response.completed --
// that .done event is what a Responses client acts on.
func TestContextManagementStreamsTheCompactionItem(t *testing.T) {
	c := &testClient{name: "stub", events: summaryEvents("folded state")}
	srv := newCompactServer(t, c)

	turns := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		turns = append(turns, `{"type":"message","role":"`+role+`","content":"`+strings.Repeat("x", 400)+`"}`)
	}
	body := `{"model":"m1","stream":true,"context_management":[{"type":"compaction","compact_threshold":100}],"input":[` + strings.Join(turns, ",") + `]}`
	rr := responses(t, srv, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	events := decodeSSE(t, rr.Body.String())

	var added, done bool
	for _, ev := range events {
		item, _ := ev["item"].(map[string]any)
		if item == nil || item["type"] != "compaction" {
			continue
		}
		switch ev["type"] {
		case "response.output_item.added":
			added = true
		case "response.output_item.done":
			done = true
			if blob, _ := item["encrypted_content"].(string); !strings.HasPrefix(blob, "c2c1.") {
				t.Fatalf("the done event carried no usable blob: %v", item["encrypted_content"])
			}
		}
	}
	if !added || !done {
		t.Fatalf("compaction item events: added=%v done=%v\n%s", added, done, rr.Body.String())
	}

	// response.completed must carry it too, or a stateless client cannot
	// replay the folded context.
	var completed map[string]any
	for _, ev := range events {
		if ev["type"] == "response.completed" {
			completed, _ = ev["response"].(map[string]any)
		}
	}
	if completed == nil {
		t.Fatalf("no response.completed event:\n%s", rr.Body.String())
	}
	items, _ := completed["output"].([]any)
	found := false
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && m["type"] == "compaction" {
			found = true
		}
	}
	if !found {
		t.Fatalf("response.completed omitted the compaction item: %v", completed)
	}
}

// One accepted request, one usage row -- including the compaction endpoint.
func TestResponsesCompactRecordsExactlyOneUsageRow(t *testing.T) {
	c := &testClient{name: "stub", events: summaryEvents("summary")}
	usage := NewUsageStore(10)
	srv := NewServer(Options{
		Registry:          regWith(c),
		Version:           "test",
		Logger:            log.New(io.Discard, "", 0),
		Stats:             NewStats(),
		Usage:             usage,
		CompactionKeyPath: filepath.Join(t.TempDir(), "compaction.key"),
	})

	rr := responsesCompact(t, srv, `{"model":"m1","input":[{"type":"message","role":"user","content":"a"},{"type":"message","role":"assistant","content":"b"},{"type":"message","role":"user","content":"c"}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if rows := usage.Snapshot(); len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1: %+v", len(rows), rows)
	}
}
