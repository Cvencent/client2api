package core

// HintContext is what a module needs to know about a failed request in order to
// say something more useful than the vendor's raw message.
//
// The gateway is the only layer that sees both the classified failure kind and
// the request that produced it, so it assembles the context and hands it to the
// module that owns the vendor vocabulary.  The zero value is legal: it means
// "nothing is known about the request", and a module must still answer.
type HintContext struct {
	// Client is the registered module name, for advice that wants to name it.
	Client string
	// Model is the bare upstream model name; the client namespace is stripped.
	Model string
	// HasImage is true when the request carried an image part.  It is the
	// premise of the "this model is image-blind" family, so it is taken from
	// the parsed request rather than guessed from the error text.
	HasImage bool
	// ModelInCatalog is true only when the module's catalogue both lists Model
	// and *states* whether it supports images.  A catalogue entry that stays
	// silent is deliberately not treated as "image-blind": inventing a
	// capability fact is worse than falling back to the neutral wording.
	ModelInCatalog bool
	// ModelSupportsImages is the catalogue's answer for Model.  It is only
	// meaningful when ModelInCatalog is true.
	ModelSupportsImages bool
}

// HintProvider is the opt-in interface a module implements to own the advice
// attached to its own upstream failures.
//
// It exists because the vendor vocabulary is per-module: workbuddy's business
// codes mean nothing to trae, and the shared rule table in internal/hint cannot
// learn them without every module's vendor leaking into it.  A module that does
// not implement this interface is unaffected -- the gateway falls back to the
// shared table exactly as before, so an unimplemented mechanism behaves the way
// it always did.
//
// Returning "" is the documented way to say "I have nothing useful for this
// one"; the gateway then falls back to the shared table rather than shipping a
// platitude.  A hint never replaces the vendor's own error.message.
type HintProvider interface {
	Hint(kind FailureKind, message string, ctx HintContext) string
}

// AsHintProvider narrows a registered client.
func AsHintProvider(c Client) (HintProvider, bool) {
	hp, ok := c.(HintProvider)
	return hp, ok
}

// HintOf returns the module's hint provider, or nil when it implements none.
func HintOf(c Client) HintProvider {
	hp, _ := AsHintProvider(c)
	return hp
}
