package workbuddy

import "client2api/internal/dsml"

// DSMLParser and dsmlCall remain aliases so the WorkBuddy stream and its tests
// share the same parser implementation as the other OpenAI-compatible clients.
type DSMLParser = dsml.Parser
type dsmlCall = dsml.Call

// NewDSMLParser returns an empty parser.
func NewDSMLParser() *DSMLParser { return dsml.NewParser() }

// HasDSMLMarker reports whether s contains a likely DSML/XML tool-call marker.
func HasDSMLMarker(s string) bool { return dsml.HasMarker(s) }
