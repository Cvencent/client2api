package minimaxcode

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ReviveAccount clears the penalty on one credential, and adopts the credential
// a fresh sign-in left in the MiniMax Code store.
//
// Clearing the penalty alone would be easy, but for this module it would be a
// lie: MiniMax retires the access token and the refresh token together -- the
// vendor answers invalid_grant / "start a new authorization" -- so a revoked
// row has nothing local left to retry.  What makes it work again is a new
// sign-in in MiniMax Code, and that sign-in lands in the auth.json this module
// already reads.  So reviving is exactly that: re-read the file and adopt what
// the operator just did.
//
// Nothing is minted here, and a store that still holds the same credential (or
// no credential at all) is reported as an error rather than a silent success:
// the panel relays that message, which is the operator's cue to sign in first.
// An id this module does not hold is an error too, so a stale browser tab
// cannot claim it revived something that is gone.
func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	_ = ctx
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("an account id is required")
	}

	c.pool.mu.Lock()
	c.pool.ensureLocked()
	target := c.pool.accountLocked(id)
	var (
		authPath string
		token    string
	)
	if target != nil {
		authPath = strings.TrimSpace(target.AuthPath)
		token = target.Token
	}
	c.pool.mu.Unlock()
	if target == nil {
		return fmt.Errorf("account %q is not held by this client", id)
	}

	// A panel-typed credential has no store behind it, so there is genuinely
	// nothing this module can do but drop the penalty and let the vendor judge
	// it again -- which is the plain Reviver contract.
	if authPath == "" {
		if c.pool.dropPenalty(id) {
			c.logf("minimaxcode: cleared the penalty on the typed credential %s", id)
			return nil
		}
		return fmt.Errorf("account %q is not held by this client", id)
	}

	cred, ok := c.readStoreCredential(Account{AuthPath: authPath})
	if !ok {
		return fmt.Errorf("MiniMax Code has no credential in %s: sign in again in the MiniMax Code client, then retry", authPath)
	}
	if cred.Access == token {
		return errors.New("MiniMax Code still holds the same credential: sign in again in the MiniMax Code client, then retry")
	}
	if !c.pool.adoptCredential(id, cred) {
		return fmt.Errorf("account %q is not held by this client", id)
	}
	c.logf("minimaxcode: revived %s from the credential the MiniMax Code client wrote", id)
	return nil
}
