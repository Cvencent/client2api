package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"client2api/internal/core"
)

// cockpitAccount is one row of the credential dump the reference implementation
// imports.
//
// The field set is the reference's, including the columns this module has no use
// for.  A dump written by that tool must keep round-tripping whether or not we
// care about every column, and a decoder that rejected unknown keys would refuse
// the next version of the exporter for no reason.
type cockpitAccount struct {
	ID             string `json:"id"`
	Email          string `json:"email"`
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	TokenType      string `json:"token_type"`
	ExpiresAt      int64  `json:"expires_at"`
	Domain         string `json:"domain"`
	DosageNotify   string `json:"dosage_notify_code"`
	PaymentType    string `json:"payment_type"`
	Status         string `json:"status"`
	UsageUpdatedAt int64  `json:"usage_updated_at"`
	LastCheckin    int64  `json:"last_checkin_time"`
	CheckinStreak  int    `json:"checkin_streak"`
	CreatedAt      int64  `json:"created_at"`
	LastUsed       int64  `json:"last_used"`
}

// cockpitDefaultLifetime is what an entry with no usable expiry is assumed to
// last.
//
// The vendor issues access tokens with a real expiry, so a dump that lost the
// column is missing information rather than announcing a dead token.  Stamping
// it as already expired would be a lie with a cost: NeedsRefresh treats a
// non-positive expiry as "unknown" and refreshes on first use, so the account
// would still work, but every report that shows an expiry would show 1970.  A
// year is the reference's choice and it is a harmless one, because the first
// successful refresh replaces the guess with the vendor's own answer.
const cockpitDefaultLifetime = 365 * 24 * time.Hour

// validImportUID reports whether a uid is safe to use as a filename component
// and as an account identifier.
//
// The charset matters beyond tidiness: the uid reaches panelFileName, so a value
// carrying "/" or ".." would decide where the credential file lands.  The length
// bound is the reference's.
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// ImportBundle implements core.BundleImporter: it ingests a credential dump
// produced by the companion cockpit tooling.
//
// The document is a JSON array in the *exporter's* shape, not this module's
// credential format, so it is decoded into its own type and translated.  That
// translation is the whole job: the dump counts expiry in milliseconds where
// this module counts seconds, it carries an email where a display name is
// wanted, and it names a domain where a realm must be inferred.
//
// Failures are per entry.  One unusable row out of a hundred must not discard
// the other ninety-nine, so a bad entry is counted in Skipped and described in
// Errors while the rest are stored.  A returned error means the document itself
// could not be read, which is the caller's problem rather than one row's.
func (c *Client) ImportBundle(ctx context.Context, name string, data []byte) (core.BundleImportReport, error) {
	var rep core.BundleImportReport
	dir := c.accountsDir()
	if dir == "" {
		return rep, errors.New("no data directory configured: an import could not be stored")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var accounts []cockpitAccount
	if err := json.Unmarshal(data, &accounts); err != nil {
		return rep, fmt.Errorf("invalid json: %w", err)
	}
	if len(accounts) == 0 {
		return rep, errors.New("the document contains no accounts")
	}
	rep.Total = len(accounts)
	if err := core.EnsureDir(dir); err != nil {
		return rep, fmt.Errorf("cannot create the accounts directory: %w", err)
	}

	// One timestamp for the whole document: two entries in the same upload must
	// not end up with expiries a millisecond apart for no reason.
	now := time.Now()
	imported := make([]*Auth, 0, len(accounts))
	for i := range accounts {
		acc := accounts[i]
		// A dump whose uid column is the missing one cannot be reported by uid,
		// so the reference names the id instead and so does this.
		label := firstNonEmpty(strings.TrimSpace(acc.ID), fmt.Sprintf("entry %d", i+1))
		skip := func(format string, args ...any) {
			rep.Skipped++
			rep.Errors = append(rep.Errors, fmt.Sprintf(format, args...))
		}

		uid := strings.TrimSpace(acc.UID)
		access := strings.TrimSpace(acc.AccessToken)
		refresh := strings.TrimSpace(acc.RefreshToken)
		if uid == "" || access == "" || refresh == "" {
			skip("missing required fields (id=%s)", label)
			continue
		}
		if !validImportUID(uid) {
			skip("invalid uid (id=%s)", label)
			continue
		}

		// The dump counts milliseconds.  Dividing is the only correct reading:
		// treating the value as seconds would place every expiry roughly fifty
		// thousand years out, and treating it as "already expired" would throw
		// away a perfectly good token.
		expiresAt := acc.ExpiresAt / 1000
		if expiresAt <= 0 {
			expiresAt = now.Add(cockpitDefaultLifetime).Unix()
		}
		domain := strings.TrimSpace(acc.Domain)
		a := &Auth{
			AccessToken:  access,
			RefreshToken: refresh,
			ExpiresAt:    expiresAt,
			Domain:       domain,
			// The realm is recorded rather than left to be inferred later, so
			// the account stays on the right vendor host even if a later edit
			// clears the domain.
			Realm: realmForDomain(domain),
			UID:   uid,
			// An empty nickname would render as a blank row in every panel
			// table; the email is a better label and is what the reference
			// falls back to.
			Nickname: firstNonEmpty(strings.TrimSpace(acc.Nickname), strings.TrimSpace(acc.Email)),
		}
		a.FilePath = c.bundleTarget(dir, uid)
		if err := a.SaveAtomic(); err != nil {
			skip("uid=%s: save auth failed: %v", uid, err)
			continue
		}
		rep.Imported++
		imported = append(imported, a)
	}

	// The pool is rebuilt from disk rather than mutated in place.  The files are
	// the source of truth for which accounts exist and refreshAccounts is the one
	// path that reads them, so going through it means an import cannot leave the
	// pool describing a set of credentials that the next reload would contradict.
	c.refreshAccounts(true)
	for _, a := range imported {
		c.afterBundleImport(ctx, a)
	}
	c.deps.Logf("workbuddy: bundle import %q total=%d imported=%d skipped=%d",
		name, rep.Total, rep.Imported, rep.Skipped)
	return rep, nil
}

// bundleTarget picks the file an imported account is stored in.
//
// The name is derived from the uid, so re-importing a newer dump overwrites the
// credential it came from instead of piling up copies.  That is what the
// reference does and what an operator means by "import this file again": the
// dump is newer information about the same account, not a second account.
//
// The case that must not overwrite is a name collision between two *different*
// accounts.  sanitizeFileComponent truncates at 48 characters, so two long uids
// sharing a prefix would otherwise silently destroy each other, and a file we
// cannot parse is likewise not evidence that it holds the account we are
// writing.  Both fall back to a suffixed name.
func (c *Client) bundleTarget(dir, uid string) string {
	target := filepath.Join(dir, panelFileName(uid))
	raw, err := os.ReadFile(target)
	if err != nil {
		return target
	}
	existing, perr := ParseAuth(raw)
	if perr != nil || existing.UID != uid {
		return filepath.Join(dir, panelFileNameSuffixed(uid, randomSuffix()))
	}
	return target
}

// realmForDomain names the vendor service a credential belongs to.
//
// It is the domain-only half of Auth.realmLocked, split out because an import
// has a domain and no realm yet: the pool decides realm by name first and
// domain second, so recording the answer here keeps the two in agreement.
func realmForDomain(domain string) string {
	if isGlobalDomain(domain) {
		return realmGlobal
	}
	return realmCN
}

// afterBundleImport runs the reference's post-import courtesy pass for one
// account: activate an international registration, claim the trial, credit
// today's check-in, then read the balance.
//
// Every step is best-effort and none of them can fail the import.  The
// credential is already on disk and the operator cannot redo the upload without
// finding the file again, so throwing that away because the vendor was briefly
// unreachable would trade a durable result for a transient one.  Each step also
// fails silently on its own: an account that is already registered, already
// claimed, or already checked in today answers "no work to do", which is a
// success with nothing to report rather than an error worth surfacing.
func (c *Client) afterBundleImport(ctx context.Context, a *Auth) {
	if a == nil {
		return
	}
	if a.IsGlobal() {
		if activated, err := c.GlobalCompleteRegistration(ctx, a); err == nil && activated {
			c.deps.Logf("workbuddy: import activated the global registration for %s", core.MaskSecret(a.UID))
		}
		if claimed, err := c.ClaimTrial(ctx, a); err == nil && claimed {
			c.deps.Logf("workbuddy: import claimed the trial for %s", core.MaskSecret(a.UID))
		}
	} else {
		c.checkinCN(ctx, a, core.CheckinResult{
			AccountID: a.ID(),
			Action:    checkinActionCN,
			At:        time.Now().UTC().Format(time.RFC3339),
		})
	}
	// The balance read is the one step with a lasting effect beyond this
	// account's own bookkeeping.  Recording it feeds the routing table -- which
	// accounts have credit, and what part of it expires when -- and, because
	// SetCreditsDetailed treats a positive balance as evidence, it is also what
	// lets an account parked for having no credit come back the moment the
	// vendor says it has some.  A zero window is passed because this pass wants
	// the totals, not a second expiring-bucket computation nobody asked for.
	remain, total, expiring, earliestAt, earliestRemaining, err :=
		c.UserResourceDetailedWithExpiry(ctx, a, 0)
	if err != nil {
		return
	}
	c.pool.SetCreditsDetailed(a, remain, total, expiring, earliestAt, earliestRemaining)
}
