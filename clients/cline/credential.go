package cline

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

// account is one stored credential plus its non-secret identity.  The JSON tags
// are the on-disk shape of accounts.json, so the file stays readable and
// hand-editable.
type account struct {
	ID           string `json:"id,omitempty"`
	Label        string `json:"label,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	Email        string `json:"email,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"`
	Note         string `json:"note,omitempty"`

	CreatedAt     int64  `json:"created_at,omitempty"`
	LastUsed      int64  `json:"last_used,omitempty"`
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

// id is the pool key.  It prefers the explicit id, then the vendor account id,
// then the JWT's own identity, and only as a last resort a token prefix --
// which is the one case where a fragment of a secret becomes a label.
func (a account) id() string {
	if s := strings.TrimSpace(a.ID); s != "" {
		return s
	}
	if s := strings.TrimSpace(a.AccountID); s != "" {
		return "acct:" + s
	}
	if s := core.JWTIdentity(bareToken(a.AccessToken)); s != "" {
		return "jwt:" + s
	}
	tok := a.AccessToken
	if len(tok) > 16 {
		tok = tok[:16]
	}
	if tok != "" {
		return "tok:" + tok
	}
	return "empty"
}

// label is the human name shown on the panel.  It never contains a token.
func (a account) label() string {
	switch {
	case a.Nickname != "" && a.AccountID != "":
		return a.Nickname + " (" + a.AccountID + ")"
	case a.Nickname != "":
		return a.Nickname
	case a.Email != "":
		return a.Email
	case a.AccountID != "":
		return a.AccountID
	case a.ID != "":
		return a.ID
	}
	return "account"
}

// expired reports whether the access token is past its expiry.  An unknown
// expiry (0) is never treated as expired: the server's 401 is the authority.
func (a account) expired(now time.Time) bool {
	if a.ExpiresAt <= 0 {
		return false
	}
	return now.Unix() >= a.ExpiresAt
}

// needsRefresh reports whether the token should be renewed before use.
func (a account) needsRefresh(now time.Time, margin time.Duration) bool {
	if a.RefreshToken == "" || a.ExpiresAt <= 0 {
		return false
	}
	return now.Add(margin).Unix() >= a.ExpiresAt
}

// usable reports whether the account may be selected right now.
func (a account) usable(now time.Time) bool {
	if a.Disabled || strings.TrimSpace(a.AccessToken) == "" {
		return false
	}
	if a.CooldownUntil > now.Unix() {
		return false
	}
	// An account that expires within the next minute is no use for a request
	// that will outlive it.
	if a.ExpiresAt > 0 && now.Add(time.Minute).Unix() >= a.ExpiresAt {
		return false
	}
	return true
}

// refreshable is true when a refresh token is present.  It is independent of
// expiry by design: a token can be non-expired and still not renewable, and
// vice versa.
func (a account) refreshable() bool { return strings.TrimSpace(a.RefreshToken) != "" }

// entry is the in-memory runtime state layered over an account.
type entry struct {
	acct  account
	state string
	until time.Time
	note  string
	fails int
}

// healthRecord is the credential-free mirror written to state.json.
type healthRecord struct {
	State         string `json:"state"`
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastUsed      int64  `json:"last_used,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
}

// --- token grants ----------------------------------------------------------

// tokenGrant is the body of POST /api/v1/auth/register and
// POST /api/v1/auth/refresh.  The field names are camelCase and the whole
// object is wrapped in {"success":…,"data":…}; see grantEnvelope.
type tokenGrant struct {
	AccessToken  string     `json:"accessToken"`
	RefreshToken string     `json:"refreshToken"`
	ExpiresAt    expiryTime `json:"expiresAt"`
	TokenType    string     `json:"tokenType"`
	UserInfo     *userInfo  `json:"userInfo"`
}

// userInfo carries the account identity.  clineUserId is the account id the
// balance endpoint wants -- NOT the JWT's sub, which is a different value with
// a different prefix and is rejected with 400.
type userInfo struct {
	ClineUserID string `json:"clineUserId"`
	Email       string `json:"email"`
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
}

// displayName is the trimmed "first last" the vendor renders.
func (u *userInfo) displayName() string {
	if u == nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
}

// grantEnvelope is the wrapper both token endpoints use.  Success is a pointer
// so "absent" is distinguishable from "false": the success test is
// `success && data.accessToken`, which a bare accessToken read would get wrong.
type grantEnvelope struct {
	Success *bool           `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
}

// expiryTime decodes the several shapes an expiry arrives in.
type expiryTime struct {
	set bool
	at  time.Time
}

// UnmarshalJSON implements json.Unmarshaler.
func (e *expiryTime) UnmarshalJSON(raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return err
		}
		if t, ok := parseTimeString(str); ok {
			e.set, e.at = true, t
		}
		return nil
	}
	// A numeric timestamp: below 1e12 it is seconds, otherwise milliseconds.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return nil
	}
	if f < 1e12 {
		e.set, e.at = true, time.Unix(int64(f), 0)
		return nil
	}
	ms := int64(f)
	e.set, e.at = true, time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond))
	return nil
}

// parseTimeString reads the ISO 8601 shapes the vendor uses.
func parseTimeString(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// unix returns the expiry in unix seconds, or 0 when unset.
func (e expiryTime) unix() int64 {
	if !e.set || e.at.IsZero() {
		return 0
	}
	return e.at.Unix()
}

// accountFromGrant builds the account a token response describes, preserving
// every identity field the response omitted.  A refresh routinely returns no
// userInfo at all, so blanking account_id/email/nickname on it would lose the
// identity the balance endpoint needs.
func accountFromGrant(prev account, g tokenGrant) account {
	next := prev
	if tok := strings.TrimSpace(g.AccessToken); tok != "" {
		next.AccessToken = normalizeToken(tok)
	}
	if rt := strings.TrimSpace(g.RefreshToken); rt != "" {
		next.RefreshToken = rt
	}
	if exp := g.ExpiresAt.unix(); exp > 0 {
		next.ExpiresAt = exp
	}
	if ui := g.UserInfo; ui != nil {
		if v := strings.TrimSpace(ui.ClineUserID); v != "" {
			next.AccountID = v
		}
		if v := strings.TrimSpace(ui.Email); v != "" {
			next.Email = v
		}
		if v := ui.displayName(); v != "" {
			next.Nickname = v
		}
	}
	// A response with no expiry at all leaves ExpiresAt at its previous value;
	// a JWT exp claim is the fallback when even that is unknown.
	if next.ExpiresAt <= 0 {
		if exp := core.JWTExpiry(bareToken(next.AccessToken)); exp > 0 {
			next.ExpiresAt = exp
		}
	}
	return next
}

// loginRecord is a device-authorisation flow in progress, persisted so a
// restart does not lose a flow the human is halfway through.
type loginRecord struct {
	URL        string `json:"url"`
	UserCode   string `json:"user_code,omitempty"`
	DeviceCode string `json:"device_code"`
	Interval   int64  `json:"interval,omitempty"`
	StartedAt  int64  `json:"started_at"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
	Message    string `json:"message,omitempty"`
}

// expired reports whether the flow is past its deadline.
func (r *loginRecord) expired(now time.Time) bool {
	return r.ExpiresAt > 0 && now.Unix() >= r.ExpiresAt
}

// pollEvery is the server-suggested interval, floored at one second and capped
// at the module's own default so a hostile value cannot stall the flow.
func (r *loginRecord) pollEvery(def time.Duration) time.Duration {
	if r.Interval <= 0 {
		return def
	}
	d := time.Duration(r.Interval) * time.Second
	if d < time.Second {
		d = time.Second
	}
	if d > def {
		d = def
	}
	return d
}
