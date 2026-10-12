package doubaoime

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"client2api/internal/core"
)

type resultSegment struct {
	Text        string
	Interim     bool
	VADFinished bool
}

func parseResultJSON(raw string) ([]resultSegment, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var envelope struct {
		Results []struct {
			Text        string `json:"text"`
			Interim     bool   `json:"is_interim"`
			VADFinished bool   `json:"is_vad_finished"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return nil, fmt.Errorf("doubaoime: parsing result_json: %w", err)
	}
	out := make([]resultSegment, 0, len(envelope.Results))
	for _, result := range envelope.Results {
		out = append(out, resultSegment{Text: result.Text, Interim: result.Interim, VADFinished: result.VADFinished})
	}
	return out, nil
}

func mergeTranscript(segments []resultSegment) (string, error) {
	var final []string
	var interim []string
	for _, segment := range segments {
		text := strings.TrimSpace(segment.Text)
		if text == "" {
			continue
		}
		if segment.Interim {
			interim = append(interim, text)
		} else {
			final = append(final, text)
		}
	}
	parts := final
	if len(parts) == 0 {
		parts = interim
	}
	var out string
	for _, part := range parts {
		out = mergePart(out, part)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", core.ErrNoSpeech
	}
	return out, nil
}

func mergePart(acc, next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return acc
	}
	if acc == "" {
		return next
	}
	if strings.HasPrefix(next, acc) {
		return next
	}
	if strings.HasPrefix(acc, next) || strings.HasSuffix(acc, next) {
		return acc
	}
	if needsEnglishSpace(acc, next) {
		return acc + " " + next
	}
	return acc + next
}

func needsEnglishSpace(left, right string) bool {
	l := []rune(left)
	r := []rune(right)
	if len(l) == 0 || len(r) == 0 {
		return false
	}
	return unicode.IsLetter(l[len(l)-1]) && l[len(l)-1] < 128 && unicode.IsLetter(r[0]) && r[0] < 128
}
