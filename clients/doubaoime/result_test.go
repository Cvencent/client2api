package doubaoime

import (
	"errors"
	"testing"

	"client2api/internal/core"
)

func TestMergeTranscriptPrefersFinalAndDeduplicatesPrefix(t *testing.T) {
	got, err := mergeTranscript([]resultSegment{
		{Text: "请帮我", Interim: true},
		{Text: "请帮我把首页"},
		{Text: "的任务打开。"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "请帮我把首页的任务打开。" {
		t.Fatalf("text = %q", got)
	}
}

func TestMergeTranscriptFallsBackToLastInterim(t *testing.T) {
	got, err := mergeTranscript([]resultSegment{{Text: "还没说", Interim: true}, {Text: "最后一段", Interim: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "还没说最后一段" {
		t.Fatalf("text = %q", got)
	}
}

func TestMergeTranscriptRejectsEmpty(t *testing.T) {
	_, err := mergeTranscript(nil)
	if !errors.Is(err, core.ErrNoSpeech) {
		t.Fatalf("error = %v", err)
	}
}

func TestParseResultJSON(t *testing.T) {
	segments, err := parseResultJSON(`{"results":[{"text":"你好","is_interim":false,"is_vad_finished":true}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 || segments[0].Text != "你好" || !segments[0].VADFinished {
		t.Fatalf("segments = %+v", segments)
	}
}
