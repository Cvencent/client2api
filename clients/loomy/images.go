package loomy

import (
	"strings"

	"client2api/internal/core"
)

// images.go owns one request-shaping rule: a model the catalogue says has no
// vision input never receives image parts on the wire.
//
// The vendor's catalogue declares each chat model's input modalities
// (capabilities.input_modalities, parsed into Extra["vision"]), and this module
// already reads it.  Forwarding the images anyway is not a graceful
// degradation, it is a failure: the text-only backends behind DeepSeek and Kimi
// flatten a base64 data URL into the prompt as raw text, so one screenshot
// costs tens of thousands of prompt tokens and a handful of them refuses the
// whole request with "内容太长了" before the model sees a single word.  Codex
// keeps every image it has ever read in its history and re-sends it on every
// turn, so one oversized screenshot poisons the rest of the session.
//
// Replacing each image with a short placeholder keeps the conversation shape --
// the model still learns an image existed and why it cannot see it -- at a cost
// of a few tokens.  Models the catalogue marks as vision-capable are untouched.

// imagePlaceholder replaces one image part.  The wording tells the model an
// image existed and why it cannot inspect it, so an agent never claims to have
// looked at pixels it never received.
const imagePlaceholder = "[image omitted: the model has no vision input]"

// acceptsImages reports whether the catalogue says the model accepts image
// input.  A model the catalogue does not know, or knows without a capability
// block, is treated as text-only: the fallback table under-reports vision on
// purpose, and an unknown id piped through Loomy's text backends is exactly the
// shape that refuses megabytes of base64.
func acceptsImages(models []core.Model, id string) bool {
	for _, m := range models {
		if !strings.EqualFold(m.ID, id) {
			continue
		}
		vision, ok := m.Extra["vision"].(bool)
		return ok && vision
	}
	return false
}

// stripImages rewrites req in place, replacing every image part with the
// placeholder text.  The rewrite never touches the caller's slices: the message
// slice and each changed part slice are rebuilt, because Chat documents that it
// copies rather than mutates the caller's request.  A request without images is
// left completely alone, slice identity included.
func stripImages(req *core.ChatRequest) {
	if req == nil {
		return
	}
	replaced := false
	msgs := make([]core.Message, len(req.Messages))
	copy(msgs, req.Messages)
	for i := range msgs {
		if !hasImagePart(msgs[i].Parts) {
			continue
		}
		replaced = true
		parts := make([]core.ContentPart, 0, len(msgs[i].Parts))
		for _, p := range msgs[i].Parts {
			if isImagePart(p) {
				parts = append(parts, core.ContentPart{Type: "text", Text: imagePlaceholder})
				continue
			}
			parts = append(parts, p)
		}
		msgs[i].Parts = parts
	}
	if replaced {
		req.Messages = msgs
	}
}

// isImagePart covers both spellings the callers use for an image part.
func isImagePart(p core.ContentPart) bool {
	return p.Type == "image_url" || p.Type == "image"
}

func hasImagePart(parts []core.ContentPart) bool {
	for _, p := range parts {
		if isImagePart(p) {
			return true
		}
	}
	return false
}
