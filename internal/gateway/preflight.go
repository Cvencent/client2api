package gateway

import (
	"context"
	"fmt"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// pessimistic pre-flight
// ---------------------------------------------------------------------------
//
// Routing used to be optimistic: the gateway sent the highest-priority
// candidate and let the module discover, one Chat call later, that the
// platform had no usable account, was parked for this model, or was already at
// its in-flight ceiling.  Only then did it record a failed candidate and move
// on.  For a bare model the operator saw a row of 候选失败 entries for
// platforms that were never really asked.
//
// Most of that answer is already in memory on this side of the wire:
//
//   - the module's own Health verdict (accounts cooling, parked, or all full),
//   - the module's optional per-model availability probe, for parks that are
//     per (account, model) and therefore outside what Health can express,
//   - the gateway's own in-flight counters, which are exactly what
//     max_in_flight and max_in_flight_per_account enforce.
//
// candidateUnavailable combines those three.  A candidate it refuses is not
// entered at all: the router demotes it for the short unavailable window and
// walks to the next platform.  The last remaining candidate is deliberately
// NOT pre-checked: a local verdict may reorder the chain between siblings, but
// it must never refuse the request.  That candidate keeps the pre-existing
// single-candidate path, which may still answer from Health without entering
// Chat.

// candidateUnavailable reports why the gateway already knows this candidate
// cannot serve the request right now, or nil when it must be tried.  It never
// calls Chat and never touches the network.
func (s *server) candidateUnavailable(ctx context.Context, c core.Candidate) error {
	return s.candidateUnavailableFor(ctx, c, nil)
}

// candidateUnavailableFor is candidateUnavailable with the request's messages,
// so a capability the request needs but the catalogue says the model lacks --
// today the image-input flag -- is caught before Chat is entered.  A nil or
// empty message list only means the caller could not say, and every check that
// reads it is skipped rather than guessed.
func (s *server) candidateUnavailableFor(ctx context.Context, c core.Candidate, msgs []core.Message) error {
	if c.Client == nil {
		return nil
	}
	// A free model is the one deliberate exception to the account-level
	// verdict: a credit-parked account may still serve it, and only the module
	// can decide that.  Health is skipped for it, but the per-model probe and
	// the gateway's own counters still apply -- a full slot is full whatever
	// the model costs.
	if !c.Free {
		if _, unavailable := moduleUnavailable(c.Client); unavailable != nil {
			return unavailable
		}
	}
	if requestHasImage(msgs) {
		if err := imageCapabilityRefusal(c); err != nil {
			return err
		}
	}
	if probe, ok := core.AsModelAvailability(c.Client); ok {
		if err := probe.Available(c.Model, c.Free); err != nil {
			return err
		}
	}
	// The gateway's own brakes, cheapest first: the whole-platform ceiling, then
	// the per-account one.  Both are the gateway's own books, so neither needs a
	// round trip.
	if s.platformAtCeiling(c.Client.Name()) {
		return fmt.Errorf("%w: %s is already at its platform in-flight ceiling",
			core.ErrBusy, c.Client.Name())
	}
	// A platform whose every usable account is already carrying
	// max_in_flight_per_account requests cannot admit this one; that is the same
	// verdict the module would return after rotating through the whole pool,
	// minus the round trip.
	if s.gatewayAccountsFull(ctx, c.Client) {
		return fmt.Errorf("%w: every account of %s is already at its in-flight ceiling",
			core.ErrBusy, c.Client.Name())
	}
	return nil
}

// imageCapabilityRefusal reports why this candidate cannot take an image the
// request carries, or nil when the catalogue either promises image input or
// says nothing.  The flag is the module's own claim (`supports_images` on the
// catalogue entry), so a silent entry stays on the optimistic path: only a
// stated false is treated as image-blind, because inventing a capability fact
// is worse than paying one failed round trip.
//
// It reads the candidate's own Extra, which resolveModelGroup and the catalogue
// paths fill from the matched entry -- no extra Models call on this path.
func imageCapabilityRefusal(c core.Candidate) error {
	if c.Client == nil {
		return nil
	}
	stated, ok := c.Extra["supports_images"]
	if !ok {
		return nil
	}
	if supports, isBool := stated.(bool); isBool && !supports {
		return fmt.Errorf("%w: %s does not support image input", core.ErrUnsupported, c.Client.Name())
	}
	return nil
}

// platformAtCeiling reports whether the operator's per-platform in-flight
// ceiling is fully taken.  Like the per-account check it only reads: no slot is
// taken and no limiter is created by asking.
func (s *server) platformAtCeiling(name string) bool {
	if s == nil || s.opts.Registry == nil || name == "" {
		return false
	}
	max := s.opts.Registry.MaxInFlightFor(name)
	if max <= 0 {
		return false
	}
	return s.trackers().PlatformInFlight(name) >= max
}

// gatewayAccountsFull reports whether the gateway's own per-account ceiling is
// saturated for every account the module lists.
//
// The ceiling belongs to the gateway, so this needs no upstream call.  The
// account census comes from Status, which every module documents as cheap and
// network-free; it is consulted only when the operator actually set a
// per-account ceiling.  A module that publishes no accounts keeps the
// historical optimistic path, because guessing on an empty census is exactly
// the kind of invented limit this check exists to avoid.
func (s *server) gatewayAccountsFull(ctx context.Context, c core.Client) bool {
	if s == nil || s.opts.Registry == nil || c == nil {
		return false
	}
	platform := c.Name()
	if s.opts.Registry.MaxInFlightPerAccountFor(platform) <= 0 {
		return false
	}
	accounts := c.Status(ctx).Accounts
	if len(accounts) == 0 {
		return false
	}
	enabled := 0
	for _, a := range accounts {
		if !a.Enabled {
			continue
		}
		enabled++
		if !s.accountAtCeiling(platform, a.ID) {
			return false
		}
	}
	return enabled > 0
}

// accountAtCeiling reports whether the gateway has already handed out every
// per-account slot for one account.  It is a read-only peek: unlike
// acquireAccountSlot it neither creates a limiter nor takes a slot, so asking
// the question cannot change the books it is asking about.
func (s *server) accountAtCeiling(platform, accountID string) bool {
	if s == nil || accountID == "" {
		return false
	}
	max := 0
	if s.opts.Registry != nil {
		max = s.opts.Registry.MaxInFlightPerAccountFor(platform)
	}
	if max <= 0 {
		return false
	}
	return s.trackers().AccountInFlight(platform, accountID) >= max
}
