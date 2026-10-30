package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// UnexposedModelError explains one whitelist refusal.
//
// A 404 from this gateway can mean three different things, and telling them
// apart is the difference between an operator fixing a policy and an operator
// hunting a phantom outage:
//
//   - the id exists on some platform but is not published.  The fix is to add
//     it to exposed_models; nothing is broken.
//   - the id exists nowhere.  The caller typed a name no platform serves, so
//     the fix is on their side.
//   - the id would resolve, but the operator has disabled it on every platform
//     that serves it.  The fix is to re-enable it.
//
// The old text -- "model %q is not exposed by this gateway" -- collapsed all
// three into one sentence that reads like the gateway is broken, and the usage
// row recorded no reason at all.  Live traffic showed the consequence: a client
// retrying an unpublished name produced a burst of instant 404s with no
// account and no duration, which looked exactly like the service dropping the
// connection.
type UnexposedModelError struct {
	Model string
	// Configured is false when the operator has no whitelist at all, in which
	// case this error can never be produced.
	Configured bool
	// Servable is true when some registered platform could route the id; the
	// request is only unpublished.
	Servable bool
	// ResolveErr is the registry's own reason when the id could not be routed.
	ResolveErr error
}

func (e *UnexposedModelError) Error() string {
	if e == nil {
		return ""
	}
	model := strings.TrimSpace(e.Model)
	switch {
	case e.Servable:
		// The name is real; only the policy hides it.  Naming the fix is what
		// keeps this from reading as an outage, while keeping the phrase "not
		// exposed" so the wire contract stays recognisable.
		return fmt.Sprintf("model %q is not exposed by this gateway: it is served by a platform but missing from exposed_models; add it there to call it, or call one of the published ids from /v1/models", model)
	case e.ResolveErr != nil:
		return fmt.Sprintf("model %q is not exposed by this gateway, and no platform can route it either: %v", model, e.ResolveErr)
	default:
		return fmt.Sprintf("model %q is not exposed by this gateway", model)
	}
}

// Unwrap exposes the registry's reason so errors.Is/As keeps working for
// callers that inspect the underlying refusal.
func (e *UnexposedModelError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.ResolveErr
}

// IsUnexposedModelError reports whether err came from the exposure whitelist.
func IsUnexposedModelError(err error) bool {
	var target *UnexposedModelError
	return errors.As(err, &target)
}

// unexposedModelError builds the diagnostic refusal for one model id.
//
// ResolveCandidates is the one authority on "can anything route this", so the
// answer is taken from it rather than re-derived: a second implementation would
// drift from the router the moment a group or an alias changed.
func (s *server) unexposedModelError(ctx context.Context, model string) error {
	out := &UnexposedModelError{Model: model}
	if s == nil || s.opts.Registry == nil {
		return out
	}
	out.Configured = s.opts.Registry.ExposedModelsConfigured()
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := s.opts.Registry.ResolveCandidates(ctx, model); err != nil {
		out.ResolveErr = err
	} else {
		out.Servable = true
	}
	return out
}
