package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

type fakeTranscriptionClient struct {
	name   string
	models []core.Model
	seen   []*core.TranscriptionRequest
	result core.TranscriptionResult
	err    error
}

func (c *fakeTranscriptionClient) Name() string { return c.name }

func (c *fakeTranscriptionClient) Models(context.Context) ([]core.Model, error) {
	return nil, nil
}

func (c *fakeTranscriptionClient) Chat(context.Context, *core.ChatRequest) (core.Stream, error) {
	return nil, core.ErrUnsupported
}

func (c *fakeTranscriptionClient) Status(context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

func (c *fakeTranscriptionClient) TranscriptionModels(context.Context) []core.Model {
	return c.models
}

func (c *fakeTranscriptionClient) Transcribe(_ context.Context, req *core.TranscriptionRequest) (core.TranscriptionResult, error) {
	c.seen = append(c.seen, req)
	return c.result, c.err
}

func newTranscriptionServer(t *testing.T, provider *fakeTranscriptionClient) *http.Server {
	t.Helper()
	reg := core.NewRegistry()
	reg.Add(provider)
	return NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
	})
}

func transcriptionRequest(t *testing.T, srv *http.Server, model, responseFormat string, audio []byte) *httptest.ResponseRecorder {
	t.Helper()
	return transcriptionRequestWithFields(t, srv, map[string]string{
		"model":           model,
		"language":        "zh",
		"response_format": responseFormat,
	}, audio, "speech.wav")
}

func transcriptionRequestWithFields(t *testing.T, srv *http.Server, fields map[string]string, audio []byte, filename string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if audio != nil {
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(audio); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range fields {
		if err := mw.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

func TestTranscriptionPassesPromptAndResponseFormat(t *testing.T) {
	provider := &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1", OwnedBy: "speech"}},
		result: core.TranscriptionResult{Text: "hello", Language: "en", Duration: 1},
	}
	srv := newTranscriptionServer(t, provider)
	rec := transcriptionRequestWithFields(t, srv, map[string]string{
		"model":           "speech/whisper-1",
		"language":        "en",
		"prompt":          "OpenAI, GPT",
		"response_format": "verbose_json",
	}, []byte("audio"), "speech.mp3")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(provider.seen) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(provider.seen))
	}
	seen := provider.seen[0]
	if seen.Prompt != "OpenAI, GPT" || seen.ResponseFormat != "verbose_json" {
		t.Fatalf("request = %+v", seen)
	}
}

func TestTranscriptionTextResponse(t *testing.T) {
	srv := newTranscriptionServer(t, &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1"}},
		result: core.TranscriptionResult{Text: "你好，世界。"},
	})
	rec := transcriptionRequest(t, srv, "whisper-1", "text", []byte("audio"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "你好，世界。" {
		t.Fatalf("body = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content type = %q", ct)
	}
}

func TestTranscriptionSRTResponse(t *testing.T) {
	srv := newTranscriptionServer(t, &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1"}},
		result: core.TranscriptionResult{
			Text:     "你好。",
			Language: "zh",
			Duration: 2.2,
			Segments: []core.TranscriptionSegment{{ID: 0, Start: 0, End: 2.2, Text: "你好。"}},
		},
	})
	rec := transcriptionRequest(t, srv, "whisper-1", "srt", []byte("audio"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	want := "1\n00:00:00,000 --> 00:00:02,200\n你好。\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/x-subrip") {
		t.Fatalf("content type = %q", ct)
	}
}

func TestTranscriptionVTTResponse(t *testing.T) {
	srv := newTranscriptionServer(t, &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1"}},
		result: core.TranscriptionResult{
			Text:     "hello",
			Language: "en",
			Duration: 1.25,
			Segments: []core.TranscriptionSegment{{ID: 0, Start: 0, End: 1.25, Text: "hello"}},
		},
	})
	rec := transcriptionRequest(t, srv, "whisper-1", "vtt", []byte("audio"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	want := "WEBVTT\n\n00:00:00.000 --> 00:00:01.250\nhello\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/vtt") {
		t.Fatalf("content type = %q", ct)
	}
}

func TestTranscriptionVerboseJSONResponse(t *testing.T) {
	srv := newTranscriptionServer(t, &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1"}},
		result: core.TranscriptionResult{
			Text:     "hello world",
			Language: "en",
			Duration: 2.5,
			Words:    []core.TranscriptionWord{{Word: "hello", Start: 0, End: 1}, {Word: "world", Start: 1.2, End: 2.5}},
			Segments: []core.TranscriptionSegment{{ID: 0, Start: 0, End: 2.5, Text: "hello world"}},
		},
	})
	rec := transcriptionRequest(t, srv, "whisper-1", "verbose_json", []byte("audio"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Task     string  `json:"task"`
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
		Text     string  `json:"text"`
		Words    []struct {
			Word  string  `json:"word"`
			Start float64 `json:"start"`
			End   float64 `json:"end"`
		} `json:"words"`
		Segments []struct {
			ID    int     `json:"id"`
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Text  string  `json:"text"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Task != "transcribe" || got.Language != "en" || got.Duration != 2.5 || got.Text != "hello world" {
		t.Fatalf("response = %+v", got)
	}
	if len(got.Words) != 2 || got.Words[1].Word != "world" || got.Words[1].Start != 1.2 {
		t.Fatalf("words = %+v", got.Words)
	}
	if len(got.Segments) != 1 || got.Segments[0].Text != "hello world" {
		t.Fatalf("segments = %+v", got.Segments)
	}
}

func TestTranscriptionRejectsUnsupportedResponseFormat(t *testing.T) {
	srv := newTranscriptionServer(t, &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1"}},
	})
	rec := transcriptionRequest(t, srv, "whisper-1", "diarized_json", []byte("audio"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestTranscriptionRejectsUnsupportedInclude(t *testing.T) {
	srv := newTranscriptionServer(t, &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1"}},
	})
	rec := transcriptionRequestWithFields(t, srv, map[string]string{
		"model":     "whisper-1",
		"include[]": "logprobs",
	}, []byte("audio"), "speech.wav")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestTranscriptionRejectsUnsupportedTimestampGranularity(t *testing.T) {
	srv := newTranscriptionServer(t, &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1"}},
	})
	rec := transcriptionRequestWithFields(t, srv, map[string]string{
		"model":                     "whisper-1",
		"response_format":           "verbose_json",
		"timestamp_granularities[]": "word",
	}, []byte("audio"), "speech.wav")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestTranscriptionRejectsUnsupportedOptions(t *testing.T) {
	srv := newTranscriptionServer(t, &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1"}},
	})
	for key, value := range map[string]string{
		"temperature":                "0.8",
		"stream":                     "true",
		"languages[]":                "en",
		"keywords[]":                 "OpenAI",
		"chunking_strategy":          "auto",
		"known_speaker_names[]":      "agent",
		"known_speaker_references[]": "data:audio/wav;base64,AAAA",
	} {
		t.Run(key, func(t *testing.T) {
			rec := transcriptionRequestWithFields(t, srv, map[string]string{
				"model": "whisper-1",
				key:     value,
			}, []byte("audio"), "speech.wav")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestTranscriptionAllowsFalseStreamAndZeroTemperature(t *testing.T) {
	provider := &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1"}},
		result: core.TranscriptionResult{Text: "hello"},
	}
	srv := newTranscriptionServer(t, provider)
	rec := transcriptionRequestWithFields(t, srv, map[string]string{
		"model":       "whisper-1",
		"stream":      "false",
		"temperature": "0",
	}, []byte("audio"), "speech.wav")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(provider.seen) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(provider.seen))
	}
}

func TestTranscriptionRoutesMultipartToProvider(t *testing.T) {
	provider := &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1", OwnedBy: "speech"}},
		result: core.TranscriptionResult{Text: "打开录音功能。", Language: "zh"},
	}
	srv := newTranscriptionServer(t, provider)

	rec := transcriptionRequest(t, srv, "speech/whisper-1", "json", []byte("RIFF....WAVE"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Text != "打开录音功能。" {
		t.Fatalf("text = %q", got.Text)
	}
	if len(provider.seen) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(provider.seen))
	}
	seen := provider.seen[0]
	if seen.Model != "whisper-1" || seen.Language != "zh" || seen.Filename != "speech.wav" {
		t.Fatalf("request = %+v", seen)
	}
	if string(seen.File) != "RIFF....WAVE" {
		t.Fatalf("file = %q", seen.File)
	}
}

func TestTranscriptionRequiresFile(t *testing.T) {
	srv := newTranscriptionServer(t, &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1"}},
	})
	rec := transcriptionRequest(t, srv, "whisper-1", "json", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestTranscriptionMapsNoSpeech(t *testing.T) {
	srv := newTranscriptionServer(t, &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1"}},
		err:    core.ErrNoSpeech,
	})
	rec := transcriptionRequest(t, srv, "whisper-1", "json", []byte("audio"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestTranscriptionMapsProviderFailure(t *testing.T) {
	srv := newTranscriptionServer(t, &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1"}},
		err:    core.Fail("speech", "", core.FailureUpstream, http.StatusGatewayTimeout, errors.New("upstream timed out")),
	})
	rec := transcriptionRequest(t, srv, "whisper-1", "json", []byte("audio"))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestModelsListsTranscriptionModels(t *testing.T) {
	srv := newTranscriptionServer(t, &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1", OwnedBy: "speech"}},
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"speech/whisper-1"`)) {
		t.Fatalf("transcription model missing from catalogue: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"transcription"`)) {
		t.Fatalf("transcription model kind missing from catalogue: %s", rec.Body.String())
	}
}
