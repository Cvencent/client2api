package loomy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"client2api/internal/core"
)

// phonecode.go implements core.PhoneCodeLoginProvider: the panel path for an
// operator who already owns a phone number.
//
// Loomy's vendor login is a plain two-step HTTP exchange -- send a code to a
// number, then redeem it -- so unlike the browser modules there is nothing for
// the operator to type into a vendor page.  The auto flow (autologin.go) rents
// a number from the one-time-SMS platform and reads the code there; this file
// is the other half: the operator names their own number, receives the text on
// their own handset, and types the code back into the panel.
//
// The pending challenges live in memory only and are dropped once used.  They
// are a one-run secret (a message id and the number it was sent to), not
// account state, so nothing here is written to disk.

const phoneCodeChallengeTTL = 5 * time.Minute

type phoneChallenge struct {
	phone     string
	msgID     string
	expiresAt time.Time
	// inFlight marks a redemption that has already claimed this challenge and
	// is talking to the vendor right now.  It keeps the one-shot guarantee --
	// a second caller arriving mid-round-trip is refused -- while leaving a
	// rejected code retypable: the entry is retired only once a redemption
	// actually succeeds.
	inFlight bool
}

var _ core.PhoneCodeLoginProvider = (*Client)(nil)

// StartPhoneCodeLogin asks the vendor to text a login code to phone and keeps
// the message id the code must be redeemed against.  The number is required:
// this flow exists precisely for a number the operator has, so an empty phone
// is a caller error rather than an instruction to rent one.
func (c *Client) StartPhoneCodeLogin(ctx context.Context, phone string) (core.PhoneCodeChallenge, error) {
	phone = strings.TrimSpace(phone)
	if !looksLikeMainlandPhone(phone) {
		return core.PhoneCodeChallenge{}, errors.New("loomy: enter an 11-digit mainland phone number")
	}
	msgID, err := c.up.sendSMSCode(ctx, phone)
	if err != nil {
		return core.PhoneCodeChallenge{}, err
	}

	now := time.Now().UTC()
	id := newPhoneChallengeID()
	ch := &phoneChallenge{phone: phone, msgID: msgID, expiresAt: now.Add(phoneCodeChallengeTTL)}

	c.phoneMu.Lock()
	if c.phoneChallenges == nil {
		c.phoneChallenges = make(map[string]*phoneChallenge)
	}
	for key, other := range c.phoneChallenges {
		if now.After(other.expiresAt) {
			delete(c.phoneChallenges, key)
		}
	}
	c.phoneChallenges[id] = ch
	c.phoneMu.Unlock()

	return core.PhoneCodeChallenge{
		ID:        id,
		ExpiresAt: ch.expiresAt.Format(time.RFC3339),
	}, nil
}

// FinishPhoneCodeLogin redeems the code the operator read and stores the
// session.  A challenge is claimed the moment a redemption starts: while one
// redemption is in flight no second caller can also redeem it, so the one-shot
// vendor code can never be spent twice.  A vendor rejection releases the claim
// and leaves the challenge live, so a mistyped code can simply be retyped; only
// a successful redemption retires the challenge.
func (c *Client) FinishPhoneCodeLogin(ctx context.Context, challengeID, code string) (core.AccountRecord, error) {
	challengeID = strings.TrimSpace(challengeID)
	code = strings.TrimSpace(code)
	if challengeID == "" {
		return core.AccountRecord{}, errors.New("loomy: the verification challenge is missing; send a code first")
	}
	if code == "" {
		return core.AccountRecord{}, errors.New("loomy: enter the verification code")
	}

	// Claim the challenge under the lock instead of merely reading it.  The
	// entry is flagged in flight (not deleted) before the vendor is called, so
	// a second caller arriving while the first is mid-round-trip is refused;
	// without the claim both callers would observe the same live entry and both
	// would spend the same one-shot code.  A vendor rejection clears the flag so
	// a mistyped code can be retyped, and only a successful redemption retires
	// the entry.
	now := time.Now().UTC()
	c.phoneMu.Lock()
	ch, live := c.phoneChallenges[challengeID]
	var expired bool
	switch {
	case !live:
		// Nothing to claim: a missing key leaves ch nil, so nothing here may
		// dereference it.
	case now.After(ch.expiresAt):
		// An expired challenge is dropped here rather than kept for a retry.
		delete(c.phoneChallenges, challengeID)
		expired = true
	case ch.inFlight:
		c.phoneMu.Unlock()
		return core.AccountRecord{}, errors.New("loomy: that code is already being checked; wait a moment and try again")
	default:
		ch.inFlight = true
	}
	c.phoneMu.Unlock()
	if !live || expired {
		if expired {
			return core.AccountRecord{}, errors.New("loomy: the verification code expired; send a new one")
		}
		return core.AccountRecord{}, errors.New("loomy: the verification challenge is unknown or already used; send a code again")
	}
	// release puts the challenge back in play after a failed redemption so the
	// operator can correct a typo without burning another text.
	release := func() {
		c.phoneMu.Lock()
		if cur, ok := c.phoneChallenges[challengeID]; ok && cur == ch {
			cur.inFlight = false
		}
		c.phoneMu.Unlock()
	}

	session, err := c.up.loginWithSMSCode(ctx, ch.phone, code, ch.msgID)
	if err != nil {
		release()
		return core.AccountRecord{}, err
	}

	record, err := c.importSession(session, time.Now().UTC())
	if err != nil {
		release()
		return core.AccountRecord{}, err
	}

	// The code is spent; retire the challenge so it can never be redeemed again.
	c.phoneMu.Lock()
	if cur, ok := c.phoneChallenges[challengeID]; ok && cur == ch {
		delete(c.phoneChallenges, challengeID)
	}
	c.phoneMu.Unlock()

	// Binding the invitation code is part of the login, not a separate
	// operator step.  It runs after the challenge is retired so a slow/broken
	// invite endpoint cannot make the successful login look retryable and spend
	// the one-shot SMS code twice.  A binding refusal is deliberately swallowed:
	// the account exists and the panel can retry the code from its row.
	_, _ = c.bindInviteAfterLogin(ctx, record.ID, c.logf)

	return record, nil
}

// looksLikeMainlandPhone accepts the 11-digit numbers the vendor accepts.  The
// account service is still the authority; this only turns an obvious typo into
// a panel error before a text can be spent.
func looksLikeMainlandPhone(phone string) bool {
	if len(phone) != 11 || phone[0] != '1' || phone[1] < '3' || phone[1] > '9' {
		return false
	}
	for i := 0; i < len(phone); i++ {
		if phone[i] < '0' || phone[i] > '9' {
			return false
		}
	}
	return true
}

func newPhoneChallengeID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "phone-" + hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000")))
	}
	return "phone-" + hex.EncodeToString(b[:])
}
