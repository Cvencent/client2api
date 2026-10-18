package gateway

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"client2api/internal/core"
)

// Prompt size estimation.
//
// There is deliberately no tokenizer here.  Vendors do not publish one, each
// family counts differently, and shipping a real BPE table would be a large
// dependency that is still wrong for every model it was not trained for.  A
// heuristic that is reliably *close*, plus the safety margin in the guard, is
// the honest tool: it only has to be good enough that the margin absorbs the
// error.
//
// The calibration:
//
//   - ASCII prose and code average roughly 4 characters per token, but runs of
//     hex, base64 and long identifiers fall closer to 2.  Dividing by 3.4 sits
//     on the dense side, which is the direction that matters: over-estimating
//     trims a little early, while under-estimating lets a doomed request
//     through and costs a full upstream round trip.
//   - CJK is close to one token per character and these tokenizers do not merge
//     ideographs, so they are counted whole.

const (
	// asciiCharsPerToken is the working divisor for non-CJK text.
	asciiCharsPerToken = 3.4
	// messageOverheadTokens is the per-message framing a vendor's chat template
	// adds: role marker, separators, turn delimiters.
	messageOverheadTokens = 4
	// toolOverheadTokens is the framing one tool declaration adds beyond its own
	// JSON.
	toolOverheadTokens = 8
	// inlineImageTokens is a conservative cost for one inline image.  Vendors
	// price images in tokens per tile rather than per byte, and over-counting only
	// makes the guard trim earlier, which is the safe direction.
	inlineImageTokens = 800
)

// inlineImageDataRE matches one base64 image data URL: the media-type prefix
// plus the base64 body that follows it.
//
// The body is the reason this needs a pattern at all.  A single 466x466 PNG
// screenshot arrives as roughly 60k base64 characters, which the plain text
// divisor reads as ~18k tokens while the vendor bills the picture by tile.
// Left uncorrected that one overcount puts the guard's estimate an order of
// magnitude ahead of the vendor's own measurement on any conversation that
// just looked at a screenshot, and every trim computed from that estimate
// lands far short of what the writer actually sends.
// inlineImageMinBase64 is the shortest base64 body treated as a picture rather
// than as text.  A real screenshot is tens of thousands of characters, so the
// threshold only stops a stray "data:image/..." substring carrying a few
// characters from being charged the full image price.
const inlineImageMinBase64 = 256

// inlineImagePrefixRE matches only the head of a data URL: the media type and
// the "base64," marker, with none of the body.
//
// The body cannot be matched in the same expression.  The base64 alphabet
// includes the letters of a following "data:" prefix, so a greedy body would
// run across the boundary between two adjacent images and swallow the head of
// the next one (Go's regexp is RE2 and has no lookahead to stop it).  The scan
// below therefore walks each body by hand and stops at the next prefix.
var inlineImagePrefixRE = regexp.MustCompile("data:image/[A-Za-z0-9.+-]+;base64,")

// isBase64Rune reports whether b can appear in a base64 body.
func isBase64Rune(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '+', b == '/', b == '=':
		return true
	}
	return false
}

// estimateTextTokens approximates the token count of one text blob.
func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	var wide, narrow int
	for _, r := range s {
		switch {
		case isWideRune(r):
			wide++
		case r < utf8.RuneSelf:
			narrow++
		default:
			// Accented Latin, Cyrillic and Greek are tokenized by bytes rather
			// than as one token per character.
			narrow += utf8.RuneLen(r)
		}
	}
	tokens := wide
	if narrow > 0 {
		tokens += int(float64(narrow)/asciiCharsPerToken + 0.5)
	}
	if tokens == 0 {
		// A blob made only of characters the switch skipped still costs a token.
		tokens = 1
	}
	return tokens
}

// estimateContentTokens approximates one message body.
//
// Plain text goes through the character heuristic.  A body carrying an inline
// base64 image is split instead: each image is priced as an image and only the
// text around it is priced as text, because the base64 alphabet is
// near-incompressible and the plain divisor turns one screenshot into tens of
// thousands of phantom tokens.
func estimateContentTokens(s string) int {
	if s == "" || !strings.Contains(s, "data:image/") {
		return estimateTextTokens(s)
	}
	heads := inlineImagePrefixRE.FindAllStringIndex(s, -1)
	if len(heads) == 0 {
		return estimateTextTokens(s)
	}
	tokens := 0
	prev := 0
	for i, head := range heads {
		bodyEnd := head[1]
		for bodyEnd < len(s) && isBase64Rune(s[bodyEnd]) {
			bodyEnd++
		}
		// A following image's head starts with "data:", whose letters are valid
		// base64; the run above would consume them.  Hand them back.
		if i+1 < len(heads) && bodyEnd > heads[i+1][0] {
			bodyEnd = heads[i+1][0]
		}
		if bodyEnd-head[1] < inlineImageMinBase64 {
			// Too short to be a picture: leave it for the text pass.
			continue
		}
		tokens += estimateTextTokens(s[prev:head[0]])
		tokens += inlineImageTokens
		prev = bodyEnd
	}
	tokens += estimateTextTokens(s[prev:])
	return tokens
}

// isWideRune reports whether r belongs to a script that costs roughly one token
// per character: CJK, kana, Hangul and fullwidth forms.
func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E, // CJK radicals, Kangxi, CJK punctuation
		r >= 0x3041 && r <= 0x33FF, // kana, Hangul compat, CJK compat
		r >= 0x3400 && r <= 0x4DBF, // CJK extension A
		r >= 0x4E00 && r <= 0x9FFF, // CJK unified ideographs
		r >= 0xA000 && r <= 0xA4CF, // Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compatibility ideographs
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD: // CJK extensions B and later
		return true
	}
	return false
}

// estimateMessageTokens approximates one conversation turn including framing.
// Tool arguments are counted because in a long agent run the context is mostly
// tool output, which is the part that grows without bound.
func estimateMessageTokens(m core.Message) int {
	n := messageOverheadTokens
	n += estimateContentTokens(m.Content)
	n += estimateTextTokens(m.Reasoning)
	n += estimateTextTokens(m.Name)
	for _, p := range m.Parts {
		if p.Type != "" && p.Type != "text" {
			n += inlineImageTokens
			continue
		}
		n += estimateContentTokens(p.Text)
	}
	for _, tc := range m.ToolCalls {
		n += estimateTextTokens(tc.Name)
		n += estimateContentTokens(tc.Arguments)
	}
	return n
}

// estimateMessagesTokens approximates a slice of turns.
func estimateMessagesTokens(msgs []core.Message) int {
	n := 0
	for _, m := range msgs {
		n += estimateMessageTokens(m)
	}
	return n
}

// estimateRequestTokens approximates the whole input side of a request.  It
// excludes MaxTokens because the caller subtracts the output reservation
// separately.
func estimateRequestTokens(req *core.ChatRequest) int {
	if req == nil {
		return 0
	}
	n := 3 // the conversation envelope itself
	n += estimateMessagesTokens(req.Messages)
	for _, t := range req.Tools {
		n += toolOverheadTokens
		n += estimateTextTokens(t.Name)
		n += estimateTextTokens(t.Description)
		n += estimateTextTokens(string(t.Parameters))
	}
	if len(req.ToolChoice) > 0 && !isNullJSON(req.ToolChoice) {
		n += estimateTextTokens(string(req.ToolChoice))
	}
	return n
}

func isNullJSON(raw []byte) bool {
	switch len(raw) {
	case 0:
		return true
	case 4:
		return string(raw) == "null"
	}
	return false
}
