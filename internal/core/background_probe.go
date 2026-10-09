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
