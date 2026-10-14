package gemini

import "errors"

// Sentinel errors the translator raises before any network call, so a caller
// can tell "this request cannot be expressed" from "the upstream refused".
var (
	errEmptyContents = errors.New("gemini: messages carried no usable content")
	errBadImage      = errors.New("gemini: image data URL is malformed")
	errRemoteImage   = errors.New("gemini: a remote image URL cannot be inlined; send it as a data URL")
)
