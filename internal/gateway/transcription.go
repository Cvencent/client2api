package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	maxTranscriptionBytes = 32 << 20
	transcriptionTimeout  = 55 * time.Second
)

type transcriptionResponse struct {
	Text string `json:"text"`
}

func (s *server) handleTranscriptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "use POST")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxTranscriptionBytes)
	if err := r.ParseMultipartForm(maxTranscriptionBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "parsing multipart form: "+err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "file is required")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxTranscriptionBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "reading file: "+err.Error())
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "file is empty")
		return
	}
	if len(data) > maxTranscriptionBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "audio file is too large")
		return
	}

	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if format := strings.ToLower(strings.TrimSpace(r.FormValue("response_format"))); format != "" && format != "json" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "response_format must be json")
		return
	}

	provider, upstreamModel, err := s.resolveTranscriptionProvider(r.Context(), model)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), transcriptionTimeout)
	defer cancel()
	result, err := provider.Transcribe(ctx, &core.TranscriptionRequest{
		File:           data,
		Filename:       header.Filename,
		ContentType:    header.Header.Get("Content-Type"),
		Model:          upstreamModel,
		Language:       strings.TrimSpace(r.FormValue("language")),
		ResponseFormat: "json",
	})
	if err != nil {
		s.writeTranscriptionError(w, provider, err)
		return
	}
	if strings.TrimSpace(result.Text) == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request_error", "no speech was recognised")
		return
	}
	writeJSON(w, http.StatusOK, transcriptionResponse{Text: result.Text})
}

func (s *server) resolveTranscriptionProvider(ctx context.Context, model string) (core.TranscriptionProvider, string, error) {
	if prefix, upstream, ok := strings.Cut(model, "/"); ok && prefix != "" && upstream != "" {
		if c, found := s.opts.Registry.Get(prefix); found {
			p, supported := c.(core.TranscriptionProvider)
			if !supported {
				return nil, "", fmt.Errorf("%w: client %q does not provide speech transcription", core.ErrUnsupported, prefix)
			}
			for _, m := range p.TranscriptionModels(ctx) {
				if strings.EqualFold(strings.TrimSpace(m.ID), upstream) {
					return p, m.ID, nil
				}
			}
			return nil, "", fmt.Errorf("%w: client %q does not serve transcription model %q", core.ErrUnsupported, prefix, upstream)
		}
	}

	for _, c := range s.opts.Registry.All() {
		p, ok := c.(core.TranscriptionProvider)
		if !ok {
			continue
		}
		for _, m := range p.TranscriptionModels(ctx) {
			if strings.EqualFold(strings.TrimSpace(m.ID), model) {
				return p, m.ID, nil
			}
		}
	}
	return nil, "", fmt.Errorf("no transcription provider serves model %q", model)
}

func (s *server) writeTranscriptionError(w http.ResponseWriter, provider core.TranscriptionProvider, err error) {
	status := http.StatusBadGateway
	typ := "upstream_error"
	switch {
	case errors.Is(err, core.ErrInvalidAudio):
		status, typ = http.StatusBadRequest, "invalid_request_error"
	case errors.Is(err, core.ErrNoSpeech):
		status, typ = http.StatusUnprocessableEntity, "invalid_request_error"
	case errors.Is(err, core.ErrNotConfigured):
		status, typ = http.StatusServiceUnavailable, "invalid_request_error"
	case errors.Is(err, core.ErrUnsupported):
		status, typ = http.StatusBadRequest, "invalid_request_error"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status, typ = http.StatusGatewayTimeout, "upstream_error"
	default:
		if f, ok := core.AsFailure(err); ok && f.Status >= 400 && f.Status <= 599 {
			status = f.Status
		}
	}
	name := "transcription"
	if provider != nil {
		name = provider.Name()
	}
	s.opts.Logger.Printf("transcription: client %s: %v", name, err)
	writeError(w, status, typ, fmt.Sprintf("[%s] %s", name, core.Redact(err.Error())))
}
