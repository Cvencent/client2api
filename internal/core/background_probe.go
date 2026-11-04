package core

import (
	"context"
	"strings"
)

// BackgroundProbeGate lets a module pace probes that are initiated by a
// background sweep, without slowing down an operator's explicit action.
//
// A module that has no vendor-specific recovery clock simply does not
// implement this interface. The shared sweeps then keep their previous
// behavior.
type BackgroundProbeGate interface {
	Client
	BackgroundProbeDue(ctx context.Context, id string) bool
}

// BackgroundProbeAllowed reports whether a background sweep may probe one
// account. Modules without a gate remain allowed so adding this contract does
// not silently disable existing recovery sweeps.
func BackgroundProbeAllowed(ctx context.Context, c Client, id string) bool {
	if c == nil {
		return false
	}
	gate, ok := c.(BackgroundProbeGate)
	if !ok {
		return true
	}
	return gate.BackgroundProbeDue(ctx, id)
}

// SelfHealableInvalid is implemented by a module whose "invalid" is NOT always
// a dead credential: part of its invalid rows carry a self-heal mark (loomy's
// abnormal -- a transient upstream fault -- and expired -- a local 14-day
// estimate), and a funded balance read proves them wrong (issues #168/#169).
//
// Implementing this lets the shared balance sweeps reach those rows, which
// every automatic path used to skip; the module's BackgroundProbeGate still
// decides whether and when each individual probe runs, so the #52 guarantee
// (a truly revoked token is never knocked twice per sweep) holds -- a dead
// session answers the probe with the vendor's own 100002 and stays parked.
//
// Modules without such a mark simply do not implement this interface, and the
// sweeps keep their previous behavior.
type SelfHealableInvalid interface {
	Client
	// InvalidSelfHeals is a marker with a name: any module that can honestly
	// answer "a balance read may clear some of my invalid rows" implements
	// it.  The method itself carries no arguments and no result -- the point
	// is that the module had to write it.
	InvalidSelfHeals()
}

// SelfHealsFromBalance reports whether c's invalid rows may be cleared by a
// balance read.  A nil client answers no.
func SelfHealsFromBalance(c Client) bool {
	if c == nil {
		return false
	}
	_, ok := c.(SelfHealableInvalid)
	return ok
}

// IsRecoverableAccountState reports whether a temporary vendor verdict can
// become usable again without re-login. These are the accounts a scheduled
// recovery probe exists to re-check; invalid credentials and operator-disabled
// accounts deliberately do not qualify.
func IsRecoverableAccountState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "cooling", "exhausted", "low_credit", "quota_exceeded", "rate_limited":
		return true
	default:
		return false
	}
}
