package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// hintBody runs a failing buffered request and returns the decoded error
// object, so a test can look at exactly one field of the envelope.
func hintBody(t *testing.T, c *testClient, body string) map[string]any {
	t.Helper()
	srv := newTestServer(t, c, nil, nil)
	rec := chat(t, srv, body)
	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a failure", rec.Code)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if inner, ok := decoded["error"].(map[string]any); ok {
		return inner
	}
	return decoded
}

const imageBody = `{"model":"m1","messages":[{"role":"user","content":[` +
	`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`

// TestTheModuleHintIsPreferredOverTheSharedTable pins the order of the two
// sources: the module that owns the vendor vocabulary answers first, because it
// is the one that can name the vendor's real reason.
func TestTheModuleHintIsPreferredOverTheSharedTable(t *testing.T) {
	c := &testClient{name: "mod", chatErr: errors.New("kaboom"), hint: "the module knows best"}
	errObj := hintBody(t, c, bufferedBody)
	if got := errObj["gateway_hint"]; got != "the module knows best" {
		t.Fatalf("gateway_hint = %v, want the module's text", got)
	}
}

// TestASilentModuleFallsBackToTheSharedTable is the hand-off half: "" from the
// module is not a verdict, so a rule that only the shared table has still gets
// its turn.  11102 is a workbuddy rule that lives there and nowhere else.
func TestASilentModuleFallsBackToTheSharedTable(t *testing.T) {
	c := &testClient{name: "workbuddy", chatErr: errors.New("upstream said 11102")}
	errObj := hintBody(t, c, bufferedBody)
	got, _ := errObj["gateway_hint"].(string)
	if got == "" {
		t.Fatalf("no hint for a message the shared table covers: %v", errObj)
	}
}

// TestNoHintAtAllOmitsTheField keeps the field out of the JSON when nobody has
// anything to say, rather than shipping an empty string to the caller.
func TestNoHintAtAllOmitsTheField(t *testing.T) {
	c := &testClient{name: "mod", chatErr: errors.New("kaboom")}
	errObj := hintBody(t, c, bufferedBody)
	if got, ok := errObj["gateway_hint"]; ok {
		t.Fatalf("gateway_hint = %v, want the key to be absent", got)
	}
}

// TestTheImageContextReachesTheModule is why the context exists at all: without
// HasImage and the catalogue facts, a module cannot tell "this model cannot see"
// from "your message was malformed".
func TestTheImageContextReachesTheModule(t *testing.T) {
	c := &testClient{
		name:    "mod",
		chatErr: errors.New("kaboom"),
		catalogue: []core.Model{
			{ID: "m1", OwnedBy: "mod", Extra: map[string]any{"supports_images": false}},
		},
	}
	if rec := chat(t, newTestServer(t, c, nil, nil), imageBody); rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a failure", rec.Code)
	}
	if c.seenHint == nil {
		t.Fatal("the module was never asked for a hint")
	}
	if !c.seenHint.HasImage {
		t.Fatal("HasImage = false for a request carrying an image part")
	}
	if !c.seenHint.ModelInCatalog || c.seenHint.ModelSupportsImages {
		t.Fatalf("catalogue facts = %+v, want in-catalogue and image-blind", *c.seenHint)
	}
	if c.seenHint.Model != "m1" {
		t.Fatalf("Model = %q, want m1", c.seenHint.Model)
	}
	if c.seenHint.Client != "mod" {
		t.Fatalf("Client = %q, want mod", c.seenHint.Client)
	}
}

// TestACatalogueThatStaysSilentIsNotACapabilityFact pins the deliberate
// asymmetry: a model listed without a supports_images flag must not be read as
// "this model cannot see images".  Guessing here would put a false statement in
// front of an end user.
func TestACatalogueThatStaysSilentIsNotACapabilityFact(t *testing.T) {
	c := &testClient{
		name:      "mod",
		chatErr:   errors.New("kaboom"),
		catalogue: []core.Model{{ID: "m1", OwnedBy: "mod"}},
	}
	if rec := chat(t, newTestServer(t, c, nil, nil), imageBody); rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a failure", rec.Code)
	}
	if c.seenHint == nil {
		t.Fatal("the module was never asked for a hint")
	}
	if c.seenHint.ModelInCatalog || c.seenHint.ModelSupportsImages {
		t.Fatalf("a silent catalogue became a capability fact: %+v", *c.seenHint)
	}
	if !c.seenHint.HasImage {
		t.Fatal("HasImage = false for a request carrying an image part")
	}
}

// TestATextOnlyRequestSkipsTheCatalogue keeps the error path cheap: only the
// image family consults the catalogue, so a text request must not pay for it.
// The fake reports an unreadable catalogue, which would set nothing anyway --
// so the assertion is that the module was asked at all, with HasImage false.
func TestATextOnlyRequestSkipsTheCatalogue(t *testing.T) {
	c := &testClient{name: "mod", chatErr: errors.New("kaboom")}
	if rec := chat(t, newTestServer(t, c, nil, nil), bufferedBody); rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a failure", rec.Code)
	}
	if c.seenHint == nil {
		t.Fatal("the module was never asked for a hint")
	}
	if c.seenHint.HasImage {
		t.Fatal("HasImage = true for a text-only request")
	}
	if c.seenHint.ModelInCatalog || c.seenHint.ModelSupportsImages {
		t.Fatalf("a text request carried catalogue facts: %+v", *c.seenHint)
	}
}

func TestRequestHasImage(t *testing.T) {
	tests := []struct {
		name string
		msgs []core.Message
		want bool
	}{
		{"no messages", nil, false},
		{"plain text", []core.Message{{Role: "user", Content: "hi"}}, false},
		{"a typed image part", []core.Message{{Role: "user", Parts: []core.ContentPart{{Type: "image_url"}}}}, true},
		{"an image url with no type", []core.Message{{Role: "user", Parts: []core.ContentPart{{ImageURL: "http://x/y.png"}}}}, true},
		{"a text part only", []core.Message{{Role: "user", Parts: []core.ContentPart{{Type: "text", Text: "hi"}}}}, false},
		{"an image in a later turn", []core.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
			{Role: "user", Parts: []core.ContentPart{{Type: "image_url", ImageURL: "http://x/y.png"}}},
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestHasImage(tc.msgs); got != tc.want {
				t.Fatalf("requestHasImage = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestModelSupportsImagesTellsNoFromNotSaid pins the three-state read: the
// second result is what separates a truthful "no" from a guess.
func TestModelSupportsImagesTellsNoFromNotSaid(t *testing.T) {
	tests := []struct {
		name       string
		extra      map[string]any
		wantValue  bool
		wantStated bool
	}{
		{"absent", nil, false, false},
		{"stated false", map[string]any{"supports_images": false}, false, true},
		{"stated true", map[string]any{"supports_images": true}, true, true},
		{"wrong type", map[string]any{"supports_images": "yes"}, false, false},
		{"unrelated keys only", map[string]any{"context_length": 8192}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			value, stated := modelSupportsImages(core.Model{ID: "m1", Extra: tc.extra})
			if value != tc.wantValue || stated != tc.wantStated {
				t.Fatalf("modelSupportsImages = (%v, %v), want (%v, %v)", value, stated, tc.wantValue, tc.wantStated)
			}
		})
	}
}

// streamData pulls the JSON payload out of every data: line of an SSE body.
func streamData(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("decode SSE payload %q: %v", payload, err)
		}
		out = append(out, m)
	}
	return out
}

// TestASuccessfulRequestNeverResolvesTheHintContext is the laziness proof.  An
// image request is the expensive case -- resolving the context reads the
// catalogue -- and a request that succeeds must not pay for that read.  The
// comparison is against a text request rather than zero because routing may
// consult the catalogue for reasons of its own; what must not happen is the
// image request reading it one extra time.  The reference gets the same
// behaviour from upstream.FrameHintFunc.
func TestASuccessfulRequestNeverResolvesTheHintContext(t *testing.T) {
	ok := []core.Event{{Type: core.EventDelta, Delta: "hi"}, {Type: core.EventDone, Finish: "stop"}}

	text := &testClient{name: "mod", events: ok}
	if rec := chat(t, newTestServer(t, text, nil, nil), bufferedBody); rec.Code != http.StatusOK {
		t.Fatalf("text status = %d, want 200", rec.Code)
	}

	img := &testClient{
		name:      "mod",
		events:    ok,
		catalogue: []core.Model{{ID: "m1", OwnedBy: "mod", Extra: map[string]any{"supports_images": false}}},
	}
	if rec := chat(t, newTestServer(t, img, nil, nil), imageBody); rec.Code != http.StatusOK {
		t.Fatalf("image status = %d, want 200", rec.Code)
	}

	if img.modelsCalls != text.modelsCalls {
		t.Fatalf("an image request read the catalogue %d times, a text one %d: the hint path is not lazy",
			img.modelsCalls, text.modelsCalls)
	}
	if img.seenHint != nil {
		t.Fatalf("a successful request asked the module for a hint: %+v", *img.seenHint)
	}
}

// TestTheStreamingErrorFrameCarriesTheHint: a stream has already answered 200 by
// the time it fails, so the hint has to ride the error frame itself -- that is
// where the reference puts it too (upstream.StreamHint).
func TestTheStreamingErrorFrameCarriesTheHint(t *testing.T) {
	c := &testClient{
		name:   "mod",
		hint:   "the module knows best",
		events: []core.Event{{Type: core.EventError, Err: errFake("upstream exploded")}},
	}
	rec := chat(t, newTestServer(t, c, nil, nil), streamBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the stream had already started)", rec.Code)
	}

	var found map[string]any
	for _, m := range streamData(t, rec.Body.String()) {
		if inner, ok := m["error"].(map[string]any); ok {
			found = inner
		}
	}
	if found == nil {
		t.Fatalf("no error frame in the stream: %q", rec.Body.String())
	}
	if got := found["gateway_hint"]; got != "the module knows best" {
		t.Fatalf("gateway_hint = %v, want the module's text", got)
	}
	if c.seenHint == nil {
		t.Fatal("the module was never asked for a hint")
	}
}

// TestAStreamingSuccessCarriesNoHint keeps the new field off the happy path: a
// successful stream must look exactly as it did before.
func TestAStreamingSuccessCarriesNoHint(t *testing.T) {
	c := &testClient{name: "mod", hint: "unused", events: usageEvents(3, 4)}
	rec := chat(t, newTestServer(t, c, nil, nil), streamBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "gateway_hint") {
		t.Fatalf("a successful stream mentioned a hint: %q", rec.Body.String())
	}
	if c.seenHint != nil {
		t.Fatalf("a successful stream asked the module for a hint: %+v", *c.seenHint)
	}
}
