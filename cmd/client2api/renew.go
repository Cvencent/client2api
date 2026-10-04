package main

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

// defaultRenewMargin is the fallback window used when a module does not tell
// the sweep how early it wants a credential renewed.  It matches the default
// refresh margin of the module this sweep was written for (CodeArts).
const defaultRenewMargin = 30 * time.Minute

// renewTimeout bounds one account's renewal so a hung vendor cannot stall the
// whole sweep.
const renewTimeout = 60 * time.Second

// refreshExpiringAccounts renews every account whose announced expiry falls
// inside that module's refresh margin.
//
// The chat path already renews a credential that is about to be used, but an
// idle account is never used, so its token would lapse while the gateway sits
// still.  This sweep is the idle half of the same rule: it asks each module
// that can manage accounts for its list, and renews the ones whose own ExpiresAt
// is close through the module's RefreshAccount.
//
// It is deliberately capability-gated and best-effort: a module that cannot
// list accounts is skipped, and one account's failure is logged without
// stopping the rest.
func refreshExpiringAccounts(ctx context.Context, reg *core.Registry, now time.Time, logger *log.Logger) {
	for _, c := range reg.All() {
		am, ok := core.AsAccountManager(c)
		if !ok {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		accounts, err := am.Accounts(lctx)
		cancel()
		if err != nil {
			logf(logger, "[%s] renewal sweep: %v", c.Name(), core.Redact(err.Error()))
			continue
		}
		for _, acct := range accounts {
			if !renewableAccount(acct) {
				continue
			}
			expiry, ok := parseAccountExpiry(acct.ExpiresAt)
			if !ok {
				continue
			}
			if expiry.After(now.Add(accountRenewMargin(acct.Fields))) {
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, renewTimeout)
			results, err := am.RefreshAccount(rctx, acct.ID)
			cancel()
			if err != nil {
				logf(logger, "[%s] renew %s: %v", c.Name(), core.MaskSecret(acct.ID), core.Redact(err.Error()))
				continue
			}
			for _, res := range results {
				if res.OK {
					continue
				}
				msg := res.Error
				if msg == "" {
					msg = "renewal did not report success"
				}
				logf(logger, "[%s] renew %s: %s", c.Name(), core.MaskSecret(res.AccountID), core.Redact(msg))
			}
		}
	}
}

// renewableAccount reports whether the sweep may touch this record at all.
//
// A parked account is the operator's decision, and an explicit
// refreshable=false means the module already knows there is no renewal path.
// An account with no announced expiry is left alone: there is nothing to renew
// against, and guessing a window would renew accounts the module considers
// permanent.
func renewableAccount(acct core.AccountRecord) bool {
	if !acct.Enabled {
		return false
	}
	if v, ok := acct.Fields["refreshable"]; ok {
		if b, isBool := v.(bool); isBool && !b {
			return false
		}
	}
	return strings.TrimSpace(acct.ExpiresAt) != ""
}

// accountRenewMargin reads the module's own refresh window out of the record.
//
// The field carries seconds because a JSON-sourced map can only hand back a
// number.  A missing or malformed value falls back to the default window rather
// than disabling the sweep, so a module that never learned to publish the field
// still gets renewed.
func accountRenewMargin(fields map[string]any) time.Duration {
	v, ok := fields["refresh_margin_seconds"]
	if !ok {
		return defaultRenewMargin
	}
	switch n := v.(type) {
	case int:
		return secondsMargin(int64(n))
	case int64:
		return secondsMargin(n)
	case float64:
		return secondsMargin(int64(n))
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return secondsMargin(i)
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return secondsMargin(i)
		}
	}
	return defaultRenewMargin
}

func secondsMargin(seconds int64) time.Duration {
	if seconds <= 0 {
		return defaultRenewMargin
	}
	return time.Duration(seconds) * time.Second
}

// parseAccountExpiry accepts the shapes modules actually publish: RFC3339,
// unix seconds and unix milliseconds.  Anything else is "unknown" and is left
// alone.
func parseAccountExpiry(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	if n >= 1_000_000_000_000 { // milliseconds
		return time.UnixMilli(n), true
	}
	return time.Unix(n, 0), true
}

func logf(logger *log.Logger, format string, args ...any) {
	if logger == nil {
		return
	}
	logger.Printf(format, args...)
}
