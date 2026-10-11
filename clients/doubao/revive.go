package doubao

import (
	"context"

	"client2api/internal/core"
)

// revive.go is the operator's "this cookie works again, clear the park" hook.
// It clears the module's own penalties; it does not mint a credential.

var _ core.Reviver = (*Client)(nil)

// ReviveAccount implements core.Reviver.
func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	return c.store.revive(id)
}
