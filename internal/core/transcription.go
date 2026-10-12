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
	ResponseFormat string
}

// TranscriptionResult is the vendor-neutral result returned to the gateway.
type TranscriptionResult struct {
	Text     string
	Language string
}

// TranscriptionProvider is the optional capability implemented by a platform
// module that can transcribe audio. It is deliberately separate from
// Client.Models: a speech model must not enter the chat routing catalogue.
type TranscriptionProvider interface {
	Client
	TranscriptionModels(ctx context.Context) []Model
	Transcribe(ctx context.Context, req *TranscriptionRequest) (TranscriptionResult, error)
}
