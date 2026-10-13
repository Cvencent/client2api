package doubaoime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"client2api/internal/core"
)

func (c *Client) Transcribe(ctx context.Context, req *core.TranscriptionRequest) (core.TranscriptionResult, error) {
	if req == nil || len(req.File) == 0 {
		return core.TranscriptionResult{}, core.ErrInvalidAudio
	}
	if strings.TrimSpace(req.Prompt) != "" {
		return core.TranscriptionResult{}, core.Fail("doubaoime", "", core.FailureOther, http.StatusBadRequest,
			fmt.Errorf("%w: prompt is not supported by the Doubao IME upstream", core.ErrUnsupported))
	}
	wav, err := decodeAudio(ctx, c.ffmpegPath, req.File)
	if err != nil {
		return core.TranscriptionResult{}, err
	}
	samples := convertToPCM16k(wav.samples, wav.sampleRate, wav.channels)
	if isSilent(samples) {
		return core.TranscriptionResult{}, core.ErrNoSpeech
	}

	totalCtx, cancel := context.WithTimeout(ctx, c.cfg.totalTimeout())
	defer cancel()
	if err := c.ensureCredentials(totalCtx, false); err != nil {
		return core.TranscriptionResult{}, err
	}
	encoder := c.encode
	if encoder == nil {
		ffmpeg := c.ffmpegPath
		encoder = func(ctx context.Context, samples []int16) ([][]byte, error) {
			return encodeWithFFmpeg(ctx, ffmpeg, samples)
		}
	}
	frames, err := encoder(totalCtx, samples)
	if err != nil {
		return core.TranscriptionResult{}, err
	}

	creds := c.credentialsSnapshot()
	text, err := c.transcribeFrames(totalCtx, creds, frames)
	if err != nil && isAuthFailure(err) {
		if refreshErr := c.ensureCredentials(totalCtx, true); refreshErr == nil {
			text, err = c.transcribeFrames(totalCtx, c.credentialsSnapshot(), frames)
		}
	}
	if err != nil {
		return core.TranscriptionResult{}, err
	}
	duration := float64(len(samples)) / 16000
	var segments []core.TranscriptionSegment
	if text != "" && duration > 0 {
		segments = []core.TranscriptionSegment{{ID: 0, Start: 0, End: duration, Text: text}}
	}
	return core.TranscriptionResult{
		Text:     text,
		Language: req.Language,
		Duration: duration,
		Segments: segments,
	}, nil
}

func (c *Client) transcribeFrames(ctx context.Context, creds cachedCredentials, frames [][]byte) (string, error) {
	if !creds.valid() {
		return "", core.ErrNotConfigured
	}
	ws, err := c.dialWebSocket(ctx, creds)
	if err != nil {
		return "", err
	}
	defer ws.Close()

	requestID, err := randomUUID()
	if err != nil {
		return "", err
	}
	if _, err := c.waitFor(ctx, ws, requestID, wsMessage{
		Token: creds.AppKey, ServiceName: "ASR", MethodName: "StartTask", RequestID: requestID,
	}, "TaskStarted", c.cfg.startTimeout()); err != nil {
		return "", err
	}

	payload, err := json.Marshal(map[string]any{
		"audio_info":              map[string]any{"channel": 1, "format": "speech_opus", "sample_rate": 16000},
		"enable_punctuation":      true,
		"enable_speech_rejection": false,
		"extra": map[string]any{
			"app_name": "com.android.chrome", "cell_compress_rate": 8, "did": creds.DeviceID,
			"enable_asr_threepass": true, "enable_asr_twopass": true, "input_mode": "tool",
		},
	})
	if err != nil {
		return "", err
	}
	if _, err := c.waitFor(ctx, ws, requestID, wsMessage{
		Token: creds.AppKey, ServiceName: "ASR", MethodName: "StartSession", RequestID: requestID, Payload: string(payload),
	}, "SessionStarted", c.cfg.startTimeout()); err != nil {
		return "", err
	}

	for i, frame := range frames {
		frameState := 3
		if i == 0 {
			frameState = 1
		}
		if i == len(frames)-1 {
			frameState = 9
		}
		framePayload, _ := json.Marshal(map[string]any{"extra": map[string]any{}, "timestamp_ms": i * 20})
		if err := c.send(ctx, ws, wsMessage{
			Token: creds.AppKey, ServiceName: "ASR", MethodName: "TaskRequest", RequestID: requestID,
			Payload: string(framePayload), AudioData: frame, FrameState: frameState,
		}); err != nil {
			return "", err
		}
	}
	if err := c.send(ctx, ws, wsMessage{
		Token: creds.AppKey, ServiceName: "ASR", MethodName: "FinishSession", RequestID: requestID,
	}); err != nil {
		return "", err
	}
	return c.readFinal(ctx, ws)
}
