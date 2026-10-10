package core

import "context"

// sms.go is the optional "rent a phone number from a one-time-SMS platform"
// capability.
//
// A module that implements SMSProvider can add an account without the operator
// owning a phone: the module asks the platform for a number, the operator types
// that number into the vendor's own login page, and the module fetches the code
// the platform received.  The panel renders the controls; the module owns the
// platform's protocol.
//
// The interface is deliberately split into four small verbs instead of one
// "add an account" call.  The browser step between them belongs to the operator
// (the vendor's login page is a cross-origin SPA), so the panel has to be able
// to interleave "acquire", "poll", "release" around it -- and a number must be
// releasable even when the login never finishes.

// SMSStatus is what the panel needs before it can render the controls.
type SMSStatus struct {
	// Configured reports whether the module holds a platform credential.  The
	// panel shows the controls either way (an operator may paste a token) but
	// says which case it is in.
	Configured bool `json:"configured"`
	// Provider names the platform, for the panel's own labelling.
	Provider string `json:"provider,omitempty"`
	// Keyword is the SMS sender keyword the platform filters on.
	Keyword string `json:"keyword,omitempty"`
	// Balance is the platform account balance, as the platform renders it.
	// Empty when it could not be read.
	Balance string `json:"balance,omitempty"`
	// Provinces is the rotation pool the module draws from.  An empty list
	// means "let the platform choose".
	Provinces []string `json:"provinces,omitempty"`
	// CardTypes are the card classes the platform accepts.
	CardTypes []string `json:"card_types,omitempty"`
	// Error is set when the module IS configured but the platform could not be
	// reached.  Configured stays true so the panel keeps its controls: the
	// problem is the platform, not the setup.
	Error string `json:"error,omitempty"`
}

// SMSOpts is one call's platform parameters.  Token, when non-empty, overrides
// the module's configured platform token -- that is how an operator can paste a
// token in the panel without editing the config file and restarting the
// process.  The rest are per-call overrides of the module's defaults.
type SMSOpts struct {
	Token string `json:"token,omitempty"`
	// Proxy overrides the HTTP transport for this SMS-platform call only.  It
	// is separate from the gateway-wide proxy because the SMS provider and the
	// vendor can need different egress paths.
	Proxy    string `json:"proxy,omitempty"`
	Keyword  string `json:"keyword,omitempty"`
	Province string `json:"province,omitempty"`
	CardType string `json:"card_type,omitempty"`
}

// SMSNumber is one rented number.
type SMSNumber struct {
	Phone    string `json:"phone"`
	Province string `json:"province,omitempty"`
	Keyword  string `json:"keyword,omitempty"`
	// Reused says the number was re-issued rather than freshly drawn: the
	// restore path asks for one specific number and the platform may or may
	// not still hold it.
	Reused bool `json:"reused,omitempty"`
}

// SMSCode is the outcome of one poll.  Ready false is the normal waiting state,
// not an error: a poll that finds nothing yet must not be retried as a failure.
type SMSCode struct {
	Ready bool   `json:"ready"`
	Code  string `json:"code,omitempty"`
	Raw   string `json:"raw,omitempty"`
}

// SMSProvider lets a module rent a one-time-SMS number and read the code it
// received.
//
// AcquirePhone must respect opts.Phone when it is set (the restore path re-issues
// the account's own number) and must skip any number in avoid.  PollSMSCode
// returns Ready false while the SMS has not arrived.  ReleasePhone hands the
// number back; block asks the platform to blacklist it instead, which the
// operator chooses when a number produced nothing but failures.
type SMSProvider interface {
	Client
	SMSStatus(ctx context.Context, opts SMSOpts) SMSStatus
	AcquirePhone(ctx context.Context, opts SMSOpts, want string, avoid []string) (SMSNumber, error)
	PollSMSCode(ctx context.Context, opts SMSOpts, phone string) (SMSCode, error)
	ReleasePhone(ctx context.Context, opts SMSOpts, phone string, block bool) error
}
