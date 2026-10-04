package cline

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"client2api/internal/core"
)

// account.go is the account-probe surface: the read-only vendor calls the panel
// makes about ONE credential (who is this, and what is left).  It is separate
// from accounts.go, which is the management surface (add, remove, refresh,
// sign in), because the two answer different questions and are used by
// different screens.

// userProfile is the shape of GET /api/v1/users/me, flattened.  The endpoint
// answers with the same userInfo object the token endpoints nest under data, so
// the two decoders accept the same spellings.
type userProfile struct {
	AccountID   string `json:"clineUserId"`
	Email       string `json:"email"`
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	DisplayName string `json:"displayName"`
	Model       string `json:"model"`
	// The raw body is kept so the balance probe can look for a credit figure
	// without this module inventing a schema the vendor never published.
	raw map[string]any
}

// whoAmI calls GET /api/v1/users/me with this credential and decodes the
// answer.  It is the cheapest proof that a credential works: one small GET, no
// model, no tokens spent.
//
// The Authorization header carries the token VERBATIM, prefix included.  A
// token with the "workos:" prefix stripped answers 401 with a body that
// misleadingly blames the client version, so the prefix is never removed here.
func (c *Client) whoAmI(ctx context.Context, acct account) (userProfile, error) {
	status, _, raw, err := c.doJSON(ctx, http.MethodGet, c.cfg.endpoint(userInfoPath), acct.AccessToken, nil)
	if err != nil {
		return userProfile{}, err
	}
	if status != http.StatusOK {
		return userProfile{}, newUpstreamError(status, errorMessage(string(raw)), nil)
	}
	return decodeProfile(raw)
}

// decodeProfile reads the user info, tolerating both the bare object and the
// {"success":true,"data":{…}} wrapper, and both the camelCase spellings the
// token endpoints use and the snake_case ones an account endpoint may use.
func decodeProfile(raw []byte) (userProfile, error) {
	body := unwrapData(raw)
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return userProfile{}, errors.New("cline: could not read the account profile: " + cleanErrorText(err.Error()))
	}
	p := userProfile{raw: doc}
	p.AccountID = pickString(doc, "clineUserId", "cline_user_id", "accountId", "account_id")
	p.Email = pickString(doc, "email", "emailAddress")
	p.FirstName = pickString(doc, "firstName", "first_name")
	p.LastName = pickString(doc, "lastName", "last_name")
	p.DisplayName = pickString(doc, "displayName", "display_name", "name")
	p.Model = pickString(doc, "model", "defaultModel", "default_model")
	if p.DisplayName == "" {
		p.DisplayName = strings.TrimSpace(p.FirstName + " " + p.LastName)
	}
	return p, nil
}

// unwrapData returns the payload of a {"success":…,"data":…} envelope, or the
// original bytes when there is no wrapper.
func unwrapData(raw []byte) []byte {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 {
		trimmed := strings.TrimSpace(string(env.Data))
		if trimmed != "" && trimmed != "null" {
			return env.Data
		}
	}
	return raw
}

// whoAmIReply renders the one-line summary TestAccount shows.
func whoAmIReply(p userProfile) string {
	switch {
	case p.DisplayName != "" && p.Email != "":
		return p.DisplayName + " <" + p.Email + ">"
	case p.Email != "":
		return p.Email
	case p.AccountID != "":
		return "account " + p.AccountID
	default:
		return "credential accepted"
	}
}

// balancePath builds the measured credit route for one account.
//
// The path carries Cline's own account id (usr-…), never the JWT subject
// (user_…).  The near-miss /api/v1/users/me/balance is a DIFFERENT route that
// answers 400 {"error":"Invalid request format"} — echoing only a per-request
// id under data.ID — for every spelling of the id as a query parameter, and 405
// for POST, so it is not the balance endpoint at all.
func balancePath(accountID string) string {
	return "/api/v1/users/" + url.PathEscape(accountID) + "/balance"
}

// balance asks the vendor what one account has left.
//
// MEASURED (live, 2026-10-02): GET /api/v1/users/{accountId}/balance answers
// {"data":{"userId":"usr-01M3VRAYHPSNWHF6VRHE9XH900","balance":496429},
// "success":true}.  The earlier implementation of this probe was a documented
// GUESS that reused GET /api/v1/users/me and scanned the profile for a credit
// figure; that endpoint carries none (it returns only id, email, displayName,
// termsAcceptedAt, clineBenchConsent, organizations, createdAt, updatedAt), so
// the probe failed for every account and the panel showed a permanent 502.
func (c *Client) balance(ctx context.Context, acct account, accountID string) (core.Balance, error) {
	id := strings.TrimSpace(accountID)
	if id == "" {
		id = strings.TrimSpace(acct.AccountID)
	}
	if id == "" {
		// The id is also readable out of the credential itself, which is where
		// the profile decoder takes it from; one extra small GET is cheaper than
		// refusing to answer for a credential that plainly names an account.
		profile, err := c.whoAmI(ctx, acct)
		if err != nil {
			return core.Balance{}, err
		}
		id = strings.TrimSpace(profile.AccountID)
	}
	if id == "" {
		return core.Balance{}, errors.New("cline: the credential names no account id, and the balance route needs one")
	}

	status, _, raw, err := c.doJSON(ctx, http.MethodGet, c.cfg.endpoint(balancePath(id)), acct.AccessToken, nil)
	if err != nil {
		return core.Balance{}, err
	}
	if status != http.StatusOK {
		return core.Balance{}, newUpstreamError(status, errorMessage(string(raw)), nil)
	}
	var doc map[string]any
	if err := json.Unmarshal(unwrapData(raw), &doc); err != nil {
		return core.Balance{}, errors.New("cline: could not read the balance: " + cleanErrorText(err.Error()))
	}
	// The vendor echoes which account it answered for; a mismatch means the
	// stored id and the credential belong to different accounts, and a figure
	// for the wrong one would be worse than no figure.
	if got := strings.TrimSpace(pickString(doc, "userId", "user_id", "id")); got != "" && got != id {
		return core.Balance{}, errors.New("the stored account id does not match the credential's account")
	}
	credits, ok := asInt64(doc["balance"])
	if !ok {
		return core.Balance{}, errors.New("the vendor's balance payload carries no credit figure")
	}
	return core.Balance{Credits: credits, Unit: "credits"}, nil
}

// asInt64 reads a number that may arrive as a JSON number or a numeric string.
func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false
	case float64:
		return int64(t), true
	case float32:
		return int64(t), true
	case int:
		return int64(t), true
	case int64:
		return t, true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, true
		}
		if f, err := t.Float64(); err == nil {
			return int64(f), true
		}
		return 0, false
	case string:
		if n, ok := asInt64String(t); ok {
			return n, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// asInt64String parses a decimal string into an integer.
func asInt64String(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	var n int64
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int64(r-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}
