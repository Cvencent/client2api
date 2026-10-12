package core

import "context"

// phonecode.go is the optional "sign in with a phone number the operator owns"
// capability.
//
// LoginProvider cannot carry this flow: it hands the operator a URL and polls,
// with no way to submit a value mid-flight.  SMSProvider only rents a number
// from a one-time-SMS platform and reads the code that platform received, so it
// cannot help an operator who receives the text on their own handset.  A module
// whose vendor texts a login code to the operator's own number therefore needs
// its own two-step call, which is what PhoneCodeLoginProvider is:
//
//	StartPhoneCodeLogin  -> ask the vendor to text a code to phone
//	FinishPhoneCodeLogin -> redeem the code the operator read and store the session
//
// The challenge is short-lived server state.  The vendor's SMS code expires, so
// a challenge that outlives it must fail rather than replay a stale message id.
// A module must hold the mapping in memory only and never persist it: it is a
// one-run secret, not an account.

// PhoneCodeChallenge is one pending operator-driven SMS login.  ExpiresAt is
// RFC3339 when the module can estimate it, empty when it cannot; the panel
// shows it only as a hint and lets the module reject a stale code.
type PhoneCodeChallenge struct {
	ID        string `json:"id"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Message   string `json:"message,omitempty"`
}

// PhoneCodeLoginProvider lets the operator add an account with a phone number
// they already own.  It is separate from SMSProvider and AutoLoginProvider on
// purpose: the module sends and redeems the vendor's code, but it never rents a
// number and never reads the code itself.
type PhoneCodeLoginProvider interface {
	Client
	// StartPhoneCodeLogin asks the vendor to text a login code to phone and
	// returns the challenge the code is redeemed against.  A caller-supplied
	// phone is mandatory: this flow exists precisely for a number the operator
	// has, so an empty phone is a caller error, not "draw one".
	StartPhoneCodeLogin(ctx context.Context, phone string) (PhoneCodeChallenge, error)
	// FinishPhoneCodeLogin redeems the code for the challenge and stores the
	// resulting account.  A vendor refusal is an error the panel can show
	// verbatim; success carries the stored record so the panel can highlight it.
	FinishPhoneCodeLogin(ctx context.Context, challengeID, code string) (AccountRecord, error)
}

// AsPhoneCodeLoginProvider narrows a registered client.  The panel answers 501
// for a module that never opted in, exactly like every other optional
// capability.
func AsPhoneCodeLoginProvider(c Client) (PhoneCodeLoginProvider, bool) {
	p, ok := c.(PhoneCodeLoginProvider)
	return p, ok
}
