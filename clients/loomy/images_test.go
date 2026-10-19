package loomy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// Loomy's own catalogue declares which chat models accept image input
// (capabilities.input_modalities, already parsed into Extra["vision"]).
// A model without that modality must never receive image parts on the wire:
// the text-only backends behind DeepSeek and Kimi read a base64 data URL as
// prompt text, so one screenshot costs tens of thousands of prompt tokens and
// a handful of them refuses the request outright ("内容太长了").  These tests
// pin both behaviours: strip for text-only models, keep for vision models.

func TestAcceptsImagesFollowsTheCatalogue(t *testing.T) {
	models := []core.Model{
		{ID: "m-text", Extra: map[string]any{"vision": false}},
		{ID: "m-vision", Extra: map[string]any{"vision": true}},
		{ID: "m-missing"},
	}
	cases := []struct {
		id   string
		want bool
	}{
		{"m-text", false},
		{"m-vision", true},
		{"m-missing", false},
		{"not-in-the-catalogue", false},
		{"M-VISION", true}, // ids resolve case-insensitively
	}
	for _, tc := range cases {
		if got := acceptsImages(models, tc.id); got != tc.want {
			t.Errorf("acceptsImages(_, %q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// The fallback table under-reports vision on purpose, so a request resolved
// against it (no live catalogue yet) must be treated as text-only.
func TestFallbackCatalogueUnderReportsVision(t *testing.T) {
	for _, m := range fallbackCatalogue() {
		if acceptsImages([]core.Model{m}, m.ID) {
			t.Errorf("fallback model %q is treated as vision-capable; the fallback table must not advertise a modality the server may reject", m.ID)
		}
	}
}

func TestStripImagesReplacesEveryImageAndKeepsTheCallerUntouched(t *testing.T) {
	req := &core.ChatRequest{Messages: []core.Message{
		{Role: "user", Parts: []core.ContentPart{
			{Type: "text", Text: "look at this"},
			{Type: "image_url", ImageURL: "data:image/png;base64,AAAA"},
		}},
		{Role: "assistant", Content: "sure"},
	}}
	original := req.Messages[0].Parts

	stripImages(req)

	parts := req.Messages[0].Parts
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want text + placeholder", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "look at this" {
		t.Fatalf("text part = %+v, want it kept in place", parts[0])
	}
	if parts[1].Type != "text" || parts[1].Text != imagePlaceholder {
		t.Fatalf("image part became %+v, want the placeholder text", parts[1])
	}
	if original[1].Type != "image_url" {
		t.Fatal("stripImages mutated the caller's own part slice")
	}
}

// A conversation without images must come out byte-identical: no reallocation,
// no rewritten messages, nothing for a reviewer to wonder about.
func TestStripImagesLeavesImageFreeRequestsAlone(t *testing.T) {
	msgs := []core.Message{
		{Role: "user", Content: "plain text only"},
		{Role: "user", Parts: []core.ContentPart{{Type: "text", Text: "array text only"}}},
	}
	req := &core.ChatRequest{Messages: msgs}

	stripImages(req)

	if &req.Messages[0] != &msgs[0] {
		t.Fatal("an image-free request had its message slice reallocated")
	}
	if len(req.Messages) != 2 || req.Messages[1].Parts[0].Text != "array text only" {
		t.Fatalf("messages changed: %+v", req.Messages)
	}
}

// End to end: a text-only model resolved from the fallback catalogue receives a
// wire body whose images are gone and whose placeholder is present.
func TestChatStripsImagesForATextOnlyModel(t *testing.T) {
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			return sseResponse("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n")
		}
		return jsonResponse(http.StatusOK, okEnvelope(``))
	}}
	c := newTestClient(t, `{"api_base":"https://loomy.test/api/v1"}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	req := &core.ChatRequest{
		Model: "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Parts: []core.ContentPart{
			{Type: "text", Text: "describe the screenshot"},
			{Type: "image_url", ImageURL: "data:image/png;base64,iVBORw0KGgo="},
		}}},
	}
	stream, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()
	if err := drainStream(stream); err != nil {
		t.Fatalf("stream: %v", err)
	}

	body := rt.lastBody()
	if strings.Contains(body, "data:image") {
		t.Fatalf("the wire body carried the base64 image: %.200s", body)
	}
	if !strings.Contains(body, imagePlaceholder) {
		t.Fatalf("the wire body lacks the placeholder: %.400s", body)
	}
	// The caller's request struct is still intact, as Chat promises.
	if req.Messages[0].Parts[1].Type != "image_url" {
		t.Fatal("Chat mutated the caller's image part")
	}
}

// A model the catalogue marks as vision-capable keeps its images: the strip is
// capability-gated, not a blanket ban on images.
func TestChatKeepsImagesForAVisionModel(t *testing.T) {
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			return sseResponse("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n")
		}
		if strings.HasSuffix(r.URL.Path, "/models") {
			return jsonResponse(http.StatusOK, okEnvelope(`[
				{"id":"m-vision","name":"M Vision · x1.0","type":"chat","context_length":123456,
				 "capabilities":{"reasoning":true,"input_modalities":["text","image"]}}
			]`))
		}
		return jsonResponse(http.StatusOK, okEnvelope(``))
	}}
	c := newTestClient(t, `{"api_base":"https://loomy.test/api/v1"}`, rt)
	seedAccount(t, c, "acc", testToken, 0)
	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model: "m-vision",
		Messages: []core.Message{{Role: "user", Parts: []core.ContentPart{
			{Type: "text", Text: "describe the screenshot"},
			{Type: "image_url", ImageURL: "data:image/png;base64,iVBORw0KGgo="},
		}}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()
	if err := drainStream(stream); err != nil {
		t.Fatalf("stream: %v", err)
	}

	body := rt.lastBody()
	if !strings.Contains(body, "data:image/png;base64,") {
		t.Fatalf("the image was stripped from a vision-capable model: %.400s", body)
	}
	if strings.Contains(body, imagePlaceholder) {
		t.Fatalf("a vision model received the placeholder: %.400s", body)
	}
}

// drain consumes a chat stream to EOF, surfacing any error frame as a failure.
func drainStream(stream core.Stream) error {
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
