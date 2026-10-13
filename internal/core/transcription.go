package core

import (
	"context"
	"errors"
)

// ErrInvalidAudio marks an upload that is not a usable audio file.
var ErrInvalidAudio = errors.New("invalid audio")

// ErrNoSpeech marks a valid audio file in which the upstream found no speech.
var ErrNoSpeech = errors.New("no speech detected")

// TranscriptionRequest is one OpenAI-compatible audio transcription request.
// File is the raw upload; the provider owns format validation and conversion.
type TranscriptionRequest struct {
	File           []byte
	Filename       string
	ContentType    string
	Model          string
	Language       string
	Prompt         string
	ResponseFormat string
}

// TranscriptionWord is one timed word in a verbose transcription result.
type TranscriptionWord struct {
	Word  string
	Start float64
	End   float64
}

// TranscriptionSegment is one timed text segment in a verbose result.
type TranscriptionSegment struct {
	ID    int
	Start float64
	End   float64
	Text  string
}

// TranscriptionResult is the vendor-neutral result returned to the gateway.
type TranscriptionResult struct {
	Text     string
	Language string
	Duration float64
	Words    []TranscriptionWord
	Segments []TranscriptionSegment
}

// TranscriptionProvider is the optional capability implemented by a platform
// module that can transcribe audio. It is deliberately separate from
// Client.Models: a speech model must not enter the chat routing catalogue.
type TranscriptionProvider interface {
	Client
	TranscriptionModels(ctx context.Context) []Model
	Transcribe(ctx context.Context, req *TranscriptionRequest) (TranscriptionResult, error)
}
