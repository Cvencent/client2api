package core

import "context"

// Reviver lets a module offer the operator an unconditional "make this account
// usable again" action.
//
// It exists because a pool's own penalties are deliberately conservative: a
// credential that reported a dead session, a plan limit, a WAF block or a
// credit exhaustion is parked for hours or until a restart, and the module has
// no legitimate reason to shorten its own verdict just because time passed.
// Only a human can say "I fixed the thing on the vendor's side, try it again".
//
// Implement it only when there is something to clear: a module with no runtime
// penalty state has nothing to revive and must NOT implement this, so the panel
// answers 501 instead of showing a button that provably does nothing.
//
// Contract:
//   - ReviveAccount removes every runtime penalty for one account: the cooldown
//     or park, the health verdict, the failure/breaker counters, and any
//     per-model park.  It also re-enables a credential the operator parked,
//     because to the operator "revive" and "enable" are the same intent.
//   - It must NOT invent, mint or refresh a credential.  A revived account with
//     a genuinely dead token is expected to fail on its next use, and that
//     failure is the evidence the pool wants.
//   - An unknown id is an error, not a silent success: the panel turns that
//     into 404 so a stale browser tab cannot claim it fixed something.
//   - Whatever the module persists must be updated too, so a revive survives a
//     restart instead of being undone by the next load from disk.
type Reviver interface {
	Client
	ReviveAccount(ctx context.Context, id string) error
}

// AsReviver narrows a registered client.  The panel uses it so a module that
// has no penalties to clear answers 501 rather than appearing broken.
func AsReviver(c Client) (Reviver, bool) {
	r, ok := c.(Reviver)
	return r, ok
}
