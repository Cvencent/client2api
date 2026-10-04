package core

import (
	"context"
	"encoding/json"
	"math"
	"strings"
)

// A model's output budget is a fact only the module that talks to the vendor
// can know, and the gateway needs it for one specific case: a caller that omits
// both max_tokens and max_completion_tokens is asking for "as much as this
// model can do".
//
// The wrong answer is expensive in a way that is hard to see.  Every module
// used to substitute its own flat default in that case -- 4096 for zcode, 32000
// for qwenwork -- which is correct for a model that advertises less and a
// silent truncation for every model that advertises more.  The caller gets a
// finish_reason of "length" for a budget it never set, in the middle of an
// answer, with nothing in the response saying who chose the number.
//
// ModelLimitsProvider is the opt-in fix, following the AsXxx convention: a
// module that knows its vendor's advertised output budget implements this one
// method and the gateway fills the request in.  A module that does not is
// unaffected and behaves exactly as it did before.
//
// It deliberately returns no error.  "This module cannot say" is a normal
// answer, not a failure, and the two must not be collapsed: an error would
// invite a module to report a lookup problem as a hard request failure, when
// the honest behaviour is to send the request with no cap at all.
type ModelLimitsProvider interface {
	Client
	// ModelMaxOutputTokens reports the largest number of tokens the vendor
	// will generate for one of this module's models.
	//
	// model is the resolved UPSTREAM id -- the same string Chat receives, not
	// the alias the caller typed -- so an implementation must not expect the
	// caller's spelling.
	//
	// ok=false means "this module cannot say", and the gateway then sends no
	// cap rather than inventing one.  Never guess: a cap that is too high is a
	// vendor-side rejection and a cap that is too low truncates silently, so
	// the only safe answer is one the vendor actually published.
	//
	// It must be cheap and side-effect free, because the gateway calls it on
	// every request that omitted max_tokens.  A module that answers from a
	// catalogue it already caches must not fetch anything here; a module whose
	// only source is the network should answer ok=false rather than block a
	// chat request on a metadata lookup.
	ModelMaxOutputTokens(ctx context.Context, model string) (int, bool)
}

// AsModelLimits narrows a registered client.
func AsModelLimits(c Client) (ModelLimitsProvider, bool) {
	ml, ok := c.(ModelLimitsProvider)
	return ml, ok
}

// ModelOutputLimit reads the advertised output budget out of one catalogue
// entry.
//
// It exists because the modules already publish this number in the same place
// -- Model.Extra["max_output_tokens"] -- but as different Go types: an int64
// from a struct-decoded vendor list, an int from a hand-built fallback table,
// and a float64 from anything that round-tripped through a map[string]any.
// Accepting all of them keeps each module's ModelMaxOutputTokens to one line.
//
// A missing key, a non-numeric value and a non-positive number are all "this
// catalogue does not say".  Zero is not a budget: a vendor that publishes 0 is
// publishing "unknown", and forwarding it as max_tokens would be a rejection
// rather than a default.
func ModelOutputLimit(m Model) (int, bool) {
	if m.Extra == nil {
		return 0, false
	}
	switch v := m.Extra["max_output_tokens"].(type) {
	case int:
		return positiveLimit(int64(v))
	case int32:
		return positiveLimit(int64(v))
	case int64:
		return positiveLimit(v)
	case float32:
		return positiveLimit(int64(v))
	case float64:
		return positiveLimit(int64(v))
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return positiveLimit(n)
	}
	return 0, false
}

// positiveLimit is the shared "a budget has to be a positive number" rule, and
// it also rejects a value that cannot survive the conversion to int -- an
// absurd number is a parsing accident, not a budget.
func positiveLimit(n int64) (int, bool) {
	if n <= 0 || n > math.MaxInt32 {
		return 0, false
	}
	return int(n), true
}

// OutputLimitFor finds a model in a catalogue and reads its budget.
//
// The id is matched exactly, because the gateway passes the resolved upstream
// id and every module keys its catalogue on that same string.  A catalogue that
// holds the id more than once (a stripped entry and a namespaced one) is
// searched to the end, so the answer does not depend on which spelling came
// first.
func OutputLimitFor(models []Model, id string) (int, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return 0, false
	}
	for _, m := range models {
		if strings.TrimSpace(m.ID) != id {
			continue
		}
		if n, ok := ModelOutputLimit(m); ok {
			return n, true
		}
	}
	return 0, false
}
