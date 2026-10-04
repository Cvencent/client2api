package core

import "context"

// ---------------------------------------------------------------------------
// Task capability (optional).
//
// Some vendors run a "growth centre": a list of daily chores the *official
// client* performs on the user's behalf (send N chat messages, adopt the
// mascot, click through a template).  A module that knows how to speak that
// protocol can offer those chores up to the panel, which then renders a task
// board instead of a bare chat endpoint.
//
// Like every other capability in core, this is discovered by a type assertion
// and nothing here is required: a module that does not implement TaskProvider
// simply reports no tasks, and CapabilitiesOf leaves the task flag false.
//
// The split of responsibility is deliberate.  Core describes *shape* only --
// what a task is, how far along it is, whether it worked.  Every vendor
// specific (task codes, progress semantics, what "run" even means) stays
// inside the module, so two modules can disagree completely and neither the
// panel nor core has to care.
// ---------------------------------------------------------------------------

// TaskInfo is one chore as the panel sees it.
//
// Current/Target are always emitted, even when zero, because 0 is a real
// progress value mid-task: a board that hides "0/5" would look identical to a
// board that has no idea what the progress is.
type TaskInfo struct {
	Code  string `json:"code"`
	Title string `json:"title,omitempty"`
	Desc  string `json:"desc,omitempty"`
	// Group is a free-form bucket ("PC", "小程序", "桌面端") the panel may
	// use to section the board.  Empty means ungrouped.
	Group     string `json:"group,omitempty"`
	Credit    int64  `json:"credit,omitempty"`
	Energy    int64  `json:"energy,omitempty"`
	Current   int64  `json:"current"`
	Target    int64  `json:"target"`
	Claimable bool   `json:"claimable,omitempty"`
	Claimed   bool   `json:"claimed,omitempty"`
	Locked    bool   `json:"locked,omitempty"`
	// Auto reports whether the module can complete this one from the panel.
	// false is not a failure: plenty of chores genuinely need a human (a real
	// connector authorisation, a donation), and saying so beats offering a
	// button that silently cannot work.
	Auto bool `json:"auto"`
	// Note carries the reason a task is not runnable, or a hint about what
	// running it will do.
	Note string `json:"note,omitempty"`
}

// TaskResult is the outcome of one run.
//
// A vendor refusal -- already claimed, prerequisite not met, anti-cheat
// rollback -- is a RESULT with OK false, not an error.  Return a non-nil error
// only when the attempt could not be made at all.
type TaskResult struct {
	OK        bool   `json:"ok"`
	Code      string `json:"code"`
	AccountID string `json:"account_id,omitempty"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
	// Credit/Energy are what actually landed this run; zero on a repeat.
	Credit    int64  `json:"credit,omitempty"`
	Energy    int64  `json:"energy,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms,omitempty"`
	At        string `json:"at,omitempty"` // RFC3339, when it finished
}

// TaskProvider lets a module expose the vendor's task board and run the
// chores it knows how to automate.
//
//	Tasks     -- read-only, must tolerate an idle pool
//	RunTask   -- the only call allowed to mutate upstream state
//
// accountID may be empty, meaning "whichever account you would use anyway";
// a module with several credentials is expected to pick one and name it in
// the result rather than refusing.
type TaskProvider interface {
	Client
	// Tasks lists the vendor's chores and their current progress.  It is
	// called whenever the panel opens the board, so a module that has to ask
	// upstream should cache briefly.  An empty slice means "nothing to show",
	// not an error.
	Tasks(ctx context.Context, accountID string) ([]TaskInfo, error)
	// RunTask performs one chore.  An unknown code is an error; a chore the
	// module deliberately does not automate returns a TaskResult with Auto
	// false semantics and a message saying why.
	RunTask(ctx context.Context, accountID, code string) (TaskResult, error)
}

// AsTaskProvider narrows a registered client.  The panel uses it so a module
// without the capability answers 501 instead of panicking.
func AsTaskProvider(c Client) (TaskProvider, bool) {
	tp, ok := c.(TaskProvider)
	return tp, ok
}

// ---------------------------------------------------------------------------
// The per-task verbs (optional, on top of TaskProvider).
//
// A single RunTask collapses four things a vendor actually does separately:
//
//	accept      mark a chore as taken on      (no reward, no progress)
//	claim       collect the reward for one    (idempotent: "already claimed")
//	auto        do what the chore describes   (then, usually, claim)
//	accept_all  take on everything outstanding at once
//
// The reference exposes all four as distinct routes, and the dashboard's task
// board has a distinct button for each, so collapsing them into one verb would
// lose the operator's ability to retry just the claim of a chore that passed.
// Each is its own interface for the same reason the rest of core is: a module
// that can run a chore but not batch-accept must be able to say so, and the
// panel must then hide exactly that button.
// ---------------------------------------------------------------------------

// TaskAccepter takes on a specific set of chores.
type TaskAccepter interface {
	Client
	// AcceptTasks marks the codes as accepted.  An unknown code is an error;
	// codes the vendor already had accepted are not (the vendor answers
	// idempotently and so must this).
	AcceptTasks(ctx context.Context, accountID string, codes []string) error
}

// BulkAccept is the outcome of taking on everything that is outstanding.
type BulkAccept struct {
	Accepted int64 `json:"accepted"`
	// Failed lists the codes the vendor refused, in the order they were tried.
	// A non-empty list is a partial success, not an error.
	Failed []string `json:"failed,omitempty"`
	// Message is the module's own summary ("所有任务均已接受" when nothing was
	// outstanding).  The panel shows it verbatim instead of inventing wording
	// for a state it cannot see.
	Message string `json:"message,omitempty"`
}

// BulkTaskAccepter takes on every outstanding chore at once.
type BulkTaskAccepter interface {
	Client
	AcceptAllTasks(ctx context.Context, accountID string) (BulkAccept, error)
}

// TaskClaim is the outcome of collecting one reward.
type TaskClaim struct {
	// AlreadyClaimed means the vendor answered "this was collected before".
	// It is a successful, idempotent outcome -- the reference answers 200 for
	// it rather than an error, and the panel says so instead of blaming the
	// operator for pressing the button twice.
	AlreadyClaimed bool   `json:"already_claimed,omitempty"`
	Credit         int64  `json:"credit,omitempty"`
	Energy         int64  `json:"energy,omitempty"`
	Message        string `json:"message,omitempty"`
}

// TaskClaimer collects one chore's reward.
type TaskClaimer interface {
	Client
	// ClaimTask claims code.  The claim itself is the call: a module must not
	// try to advance the chore first, because the operator pressed "claim"
	// precisely to retry a reward whose progress is already complete.
	ClaimTask(ctx context.Context, accountID, code string) (TaskClaim, error)
}

// AutoTaskResult is one automated run with the read-back the operator needs to
// judge it: progress before and after, whether the chore is now claimable, and
// what the automatic claim yielded.  Every field is optional; a module that
// cannot read progress back simply leaves the strings empty.
type AutoTaskResult struct {
	TaskResult
	// Skipped means the chore was already rewarded and nothing was attempted.
	Skipped bool `json:"skipped,omitempty"`
	// ProgressBefore/After are display strings ("3/5", "claimed"); core does
	// not interpret them.
	ProgressBefore string `json:"progress_before,omitempty"`
	ProgressAfter  string `json:"progress_after,omitempty"`
	// Claimable is the read-back verdict: the chore is complete and its reward
	// is waiting.
	Claimable bool `json:"claimable,omitempty"`
	// Attempt reports whether the module actually exercised the chore.
	Attempt bool `json:"attempt,omitempty"`
	// Claimed reports that the module collected the reward itself at the end of
	// a successful run.  The amounts live on the embedded TaskResult (Credit,
	// Energy) -- they are deliberately not repeated here, because an outer
	// field of the same name would shadow the embedded one and silently drop
	// whatever the module set there.
	Claimed bool `json:"claimed,omitempty"`
	// ClaimError is set when the chore was completed but the reward could not
	// be collected: the operator can then press claim by hand.
	ClaimError string `json:"claim_error,omitempty"`
}

// TaskAutoRunner automates one chore and reports the read-back.
type TaskAutoRunner interface {
	Client
	// RunTaskAuto performs the chore's automation.  A chore the module cannot
	// automate is an error naming the reason: the reference answers 501 so the
	// panel can tell "go do this in the official client" apart from a failure.
	RunTaskAuto(ctx context.Context, accountID, code string) (AutoTaskResult, error)
}

// AsTaskAccepter, AsBulkTaskAccepter, AsTaskClaimer and AsTaskAutoRunner
// narrow a registered client for the four verbs above.
func AsTaskAccepter(c Client) (TaskAccepter, bool) {
	v, ok := c.(TaskAccepter)
	return v, ok
}

// AsBulkTaskAccepter narrows a registered client.
func AsBulkTaskAccepter(c Client) (BulkTaskAccepter, bool) {
	v, ok := c.(BulkTaskAccepter)
	return v, ok
}

// AsTaskClaimer narrows a registered client.
func AsTaskClaimer(c Client) (TaskClaimer, bool) {
	v, ok := c.(TaskClaimer)
	return v, ok
}

// AsTaskAutoRunner narrows a registered client.
func AsTaskAutoRunner(c Client) (TaskAutoRunner, bool) {
	v, ok := c.(TaskAutoRunner)
	return v, ok
}
