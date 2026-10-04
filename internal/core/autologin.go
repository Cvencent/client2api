package core

import "context"

// autologin.go is the optional "run the vendor's browser login for me"
// capability.
//
// The ordinary login flow (LoginProvider) hands the operator an authorisation
// URL and waits: the vendor's page is a cross-origin SPA, so the operator has
// to open it, type the rented number and paste the SMS code back.  A module
// that implements AutoLoginProvider instead drives a real browser itself --
// rent a number, fill the vendor's form, fetch the code from the SMS platform,
// submit -- and reports progress while it runs.
//
// The flow is a JOB rather than a call because it takes minutes: StartAutoLogin
// returns as soon as the run is accepted, PollAutoLogin is what the panel
// renders while it works, and CancelAutoLogin stops a run the operator no
// longer wants.  The panel owns the polling cadence, exactly as it does for
// LoginProvider.

// Auto-login job states.  Running is the only non-terminal one.
const (
	AutoLoginRunning   = "running"
	AutoLoginSuccess   = "success"
	AutoLoginFailed    = "failed"
	AutoLoginCancelled = "cancelled"
)

// AutoLogLine is one progress line.  At is RFC3339 so the panel can render a
// timeline without guessing at a timezone.
type AutoLogLine struct {
	At   string `json:"at"`
	Text string `json:"text"`
}

// AutoLoginJob is one auto-login run as the panel sees it.  Log is append-only
// and in order, so a poller can render just the lines it has not shown yet.
type AutoLoginJob struct {
	ID    string        `json:"id"`
	State string        `json:"state"`
	Realm string        `json:"realm,omitempty"`
	Phone string        `json:"phone,omitempty"`
	Step  string        `json:"step,omitempty"`
	Log   []AutoLogLine `json:"log"`
	// Message is the operator-facing outcome on failure, and the vendor's own
	// wording on success.
	Message string `json:"message,omitempty"`
	// AccountID is set once the credential has been stored, so the panel can
	// highlight the row it just added.
	AccountID  string `json:"account_id,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// AutoLoginRequest is one run's parameters.  Every field is optional: an empty
// realm means the module's configured default, and empty SMS fields mean the
// module's configured platform settings.  Token overrides the module's stored
// platform credential for this run only, the same way SMSOpts does.
type AutoLoginRequest struct {
	Realm    string `json:"realm,omitempty"`
	Token    string `json:"token,omitempty"`
	Keyword  string `json:"keyword,omitempty"`
	Province string `json:"province,omitempty"`
	CardType string `json:"card_type,omitempty"`
	// Phone pins the number to rent.  Empty means "draw one", which is the
	// normal path; the restore flow sets it to the account's own number.
	Phone string `json:"phone,omitempty"`
}

// AutoLoginProvider runs a vendor login end to end in a browser the module
// drives itself.
//
// StartAutoLogin must return as soon as the run is accepted and must fail fast
// on a missing prerequisite (no browser, no platform credential) rather than
// accepting a job that can only fail.  PollAutoLogin must be safe to call
// repeatedly and must keep answering after the run finishes, so a poller that
// was asleep during the transition still sees the outcome.
type AutoLoginProvider interface {
	Client
	StartAutoLogin(ctx context.Context, req AutoLoginRequest) (AutoLoginJob, error)
	PollAutoLogin(ctx context.Context, id string) (AutoLoginJob, error)
	CancelAutoLogin(ctx context.Context, id string) error
}
