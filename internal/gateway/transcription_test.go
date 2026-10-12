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
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if audio != nil {
		part, err := mw.CreateFormFile("file", "speech.wav")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(audio); err != nil {
			t.Fatal(err)
		}
	}
	_ = mw.WriteField("model", model)
	_ = mw.WriteField("language", "zh")
	_ = mw.WriteField("response_format", responseFormat)
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
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
