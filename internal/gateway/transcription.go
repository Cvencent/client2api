package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
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

type transcriptionVerboseResponse struct {
	Task     string                 `json:"task"`
	Language string                 `json:"language"`
	Duration float64                `json:"duration"`
	Text     string                 `json:"text"`
	Words    []transcriptionWord    `json:"words,omitempty"`
	Segments []transcriptionSegment `json:"segments,omitempty"`
}

type transcriptionWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type transcriptionSegment struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
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
	responseFormat := strings.ToLower(strings.TrimSpace(r.FormValue("response_format")))
	if responseFormat == "" {
		responseFormat = "json"
	}
	if !supportedTranscriptionResponseFormat(responseFormat) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "response_format must be one of json, text, srt, verbose_json or vtt")
		return
	}
	if format := strings.TrimSpace(r.FormValue("response_format")); format == "" {
		// An explicitly empty value is the documented default.
		responseFormat = "json"
	}
	if strings.TrimSpace(r.FormValue("include[]")) != "" || strings.TrimSpace(r.FormValue("include")) != "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "include is not supported by this transcription model")
		return
	}
	if strings.TrimSpace(r.FormValue("timestamp_granularities")) != "" || strings.TrimSpace(r.FormValue("timestamp_granularities[]")) != "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "timestamp_granularities is not supported by this transcription model")
		return
	}
	if stream := strings.TrimSpace(r.FormValue("stream")); stream != "" && !strings.EqualFold(stream, "false") {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "stream=true is not supported by this transcription model")
		return
	}
	if temperature := strings.TrimSpace(r.FormValue("temperature")); temperature != "" {
		value, err := strconv.ParseFloat(temperature, 64)
		if err != nil || value != 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "temperature is not supported by this transcription model")
			return
		}
	}
	if unsupportedFormValue(r, "languages[]", "languages", "keywords[]", "keywords",
		"chunking_strategy", "known_speaker_names[]", "known_speaker_names",
		"known_speaker_references[]", "known_speaker_references") {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "one or more requested transcription options are not supported by this transcription model")
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
		Prompt:         strings.TrimSpace(r.FormValue("prompt")),
		ResponseFormat: responseFormat,
	})
	if err != nil {
		s.writeTranscriptionError(w, provider, err)
		return
	}
	if strings.TrimSpace(result.Text) == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request_error", "no speech was recognised")
		return
	}
	writeTranscriptionResult(w, responseFormat, result)
}

func supportedTranscriptionResponseFormat(format string) bool {
	switch format {
	case "json", "text", "srt", "verbose_json", "vtt":
		return true
	default:
		return false
	}
}

func unsupportedFormValue(r *http.Request, keys ...string) bool {
	for _, key := range keys {
		if strings.TrimSpace(r.FormValue(key)) != "" {
			return true
		}
	}
	return false
}

func writeTranscriptionResult(w http.ResponseWriter, format string, result core.TranscriptionResult) {
	switch format {
	case "json":
		writeJSON(w, http.StatusOK, transcriptionResponse{Text: result.Text})
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, result.Text)
	case "verbose_json":
		segments := make([]transcriptionSegment, 0, len(result.Segments))
		for _, segment := range result.Segments {
			segments = append(segments, transcriptionSegment{ID: segment.ID, Start: segment.Start, End: segment.End, Text: segment.Text})
		}
		words := make([]transcriptionWord, 0, len(result.Words))
		for _, word := range result.Words {
			words = append(words, transcriptionWord{Word: word.Word, Start: word.Start, End: word.End})
		}
		writeJSON(w, http.StatusOK, transcriptionVerboseResponse{
			Task: "transcribe", Language: result.Language, Duration: result.Duration,
			Text: result.Text, Words: words, Segments: segments,
		})
	case "srt", "vtt":
		writeTranscriptionCaptions(w, format, result)
	}
}

func writeTranscriptionCaptions(w http.ResponseWriter, format string, result core.TranscriptionResult) {
	segments := result.Segments
	if len(segments) == 0 {
		end := result.Duration
		if end <= 0 {
			end = 0
		}
		segments = []core.TranscriptionSegment{{ID: 0, Start: 0, End: end, Text: result.Text}}
	}
	sort.SliceStable(segments, func(i, j int) bool { return segments[i].Start < segments[j].Start })
	var out strings.Builder
	if format == "vtt" {
		out.WriteString("WEBVTT\n\n")
	} else {
		w.Header().Set("Content-Type", "application/x-subrip; charset=utf-8")
	}
	for i, segment := range segments {
		if format == "srt" {
			fmt.Fprintf(&out, "%d\n", i+1)
		}
		out.WriteString(captionTimestamp(segment.Start, format == "vtt"))
		out.WriteString(" --> ")
		out.WriteString(captionTimestamp(segment.End, format == "vtt"))
		out.WriteByte('\n')
		out.WriteString(strings.TrimSpace(segment.Text))
		out.WriteString("\n\n")
	}
	if format == "vtt" {
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, out.String())
}

func captionTimestamp(seconds float64, vtt bool) string {
	if seconds < 0 {
		seconds = 0
	}
	millis := int64(seconds*1000 + 0.5)
	hours := millis / 3600000
	millis %= 3600000
	minutes := millis / 60000
	millis %= 60000
	secs := millis / 1000
	ms := millis % 1000
	separator := ","
	if vtt {
		separator = "."
	}
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", hours, minutes, secs, separator, ms)
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
