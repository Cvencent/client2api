package codearts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"client2api/internal/core"
)

// accounts.go holds the credential shape and the on-disk credential store.
//
// The panel surface that exposes it (core.AccountManager, core.LoginProvider,
// core.CheckinProvider, ...) lives in panel.go; this file is only about the
// data and the rules for reading and writing it.
//
// Credential identity
// -------------------
// Every other module in this repository identifies an account by its access
// token.  CodeArts is the exception: a credential here is an IAM
// access_key_id / secret_access_key pair, and the reference implementation
// makes the same choice — `findAccountIdByCredential` uses `access_key_id`
// when the provider is codearts and `access_token` for everybody else.  The
// account id below is therefore derived from the access key id, and
// core.AccountRecord.Identity carries it so the panel can group a stored
// credential with a config-supplied one for the same key.

const (
	// accountsVersion is the schema version of accounts.json.  A file with a
	// higher version is left alone rather than half-read.
	accountsVersion = 1

	// maxAccessKeyBytes / maxSecretKeyBytes bound what a human can paste.
	maxAccessKeyBytes = 256
	maxSecretKeyBytes = 512
	maxTokenBytes     = 8192
	maxLabelRunes     = 120
)

// account is one CodeArts credential.
//
// There are two shapes.  A full OAuth credential carries a security token and
// a refresh token and works against everything.  A bare AK/SK pair carries no
// security token, which means it can sign a plain SDK-HMAC-SHA256 request but
// cannot authenticate to the chat API — that endpoint wants the temporary
// security token inside the signature.  `usable()` says which one it is.
type account struct {
	ID              string `json:"id,omitempty"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token,omitempty"`
	RefreshToken    string `json:"refresh_token,omitempty"`
	ExpiresAt       int64  `json:"expires_at,omitempty"` // unix seconds; 0 = unknown
	DomainID        string `json:"domain_id,omitempty"`
	UserID          string `json:"user_id,omitempty"`
	UserName        string `json:"user_name,omitempty"`
	Note            string `json:"note,omitempty"`

	// Disabled is the operator's park switch.  It is deliberately inverted
	// relative to core.AccountSpec.Enabled so that the zero value means
	// "enabled", which is what a credential pasted by hand should be.
	Disabled bool `json:"disabled,omitempty"`

	// DpopPrivateJwk is the ES256 private key the credential was minted with.
	// It is required to refresh the credential: the token endpoint rejects a
	// refresh whose DPoP proof is signed by a different key.
	DpopPrivateJwk string `json:"dpop_private_key_jwk,omitempty"`
	// CodeVerifier is the PKCE verifier paired with the refresh token.
	CodeVerifier string `json:"code_verifier,omitempty"`

	CreatedAt int64 `json:"created_at,omitempty"`
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// accountsStore is the JSON document in accounts.json.
type accountsStore struct {
	Version  int       `json:"version"`
	Accounts []account `json:"accounts"`
}

// accountID derives the stable id for a credential.  The access key id is the
// vendor's own identifier for the key pair, so it is both stable and
// meaningful; a credential without one falls back to the user id and finally
// to the access key id.
func (a account) accountID() string {
	if v := strings.TrimSpace(a.ID); v != "" {
		return v
	}
	if v := strings.TrimSpace(a.AccessKeyID); v != "" {
		return v
	}
	if v := strings.TrimSpace(a.UserID); v != "" {
		return "user:" + strings.TrimSpace(a.UserID)
	}
	return ""
}

// identity is what core.AccountRecord.Identity carries.  Two records with the
// same identity are the same vendor account reached by different routes, so
// the access key id is exactly the right value: the config file and a stored
// credential that name the same key describe one account, not two.
func (a account) identity() string {
	return strings.TrimSpace(a.AccessKeyID)
}

// label is what the panel shows.
func (a account) label() string {
	if v := strings.TrimSpace(a.Note); v != "" {
		return v
	}
	if v := strings.TrimSpace(a.UserName); v != "" {
		return v
	}
	if v := strings.TrimSpace(a.AccessKeyID); v != "" {
		return "AK " + maskKey(v)
	}
	return "codearts"
}

// maskKey renders an access key id so it can appear in a log line or a label
// without leaking the key.  An access key id is not a secret on its own, but
// it identifies the account, so it is still truncated.
func maskKey(k string) string {
	k = strings.TrimSpace(k)
	if len(k) <= 8 {
		return "***"
	}
	return k[:4] + "…" + k[len(k)-4:]
}

// fingerprint is an irreversible per-credential tag.  The panel can use it to
// tell two credentials apart without either of them being recoverable.
func (a account) fingerprint() string {
	sum := sha256.Sum256([]byte(a.AccessKeyID + "\x00" + a.SecretAccessKey))
	return hex.EncodeToString(sum[:4])
}

// usable reports whether the credential can authenticate at all.  A security
// token is what the chat API signs with; without one the credential can still
// list models but every completion will be refused.
func (a account) usable() bool {
	return strings.TrimSpace(a.AccessKeyID) != "" && strings.TrimSpace(a.SecretAccessKey) != ""
}

// refreshable reports whether the credential can renew itself.
func (a account) refreshable() bool {
	return a.usable() && strings.TrimSpace(a.RefreshToken) != "" && strings.TrimSpace(a.DpopPrivateJwk) != ""
}

// expired reports whether the credential's own expiry has passed.  A zero
// ExpiresAt means "unknown", which is never treated as expired: refusing a
// credential the vendor would still accept is worse than one wasted request.
func (a account) expired(now time.Time) bool {
	if a.ExpiresAt <= 0 {
		return false
	}
	return now.Unix() >= a.ExpiresAt
}

// expiringWithin reports whether the credential expires inside d.  It is what
// drives the proactive refresh in the pool.
func (a account) expiringWithin(now time.Time, d time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return false
	}
	return now.Add(d).Unix() >= a.ExpiresAt
}

// expiresAtString renders the expiry for a panel record.
func (a account) expiresAtString() string {
	if a.ExpiresAt <= 0 {
		return ""
	}
	return time.Unix(a.ExpiresAt, 0).UTC().Format(time.RFC3339)
}

// mergeAccount overlays b onto a, keeping a's values wherever b is empty.  It
// is how a stored credential and a config-supplied credential for the same key
// are combined without either erasing the other's fields.
func mergeAccount(a, b account) account {
	if v := strings.TrimSpace(b.ID); v != "" {
		a.ID = v
	}
	if v := strings.TrimSpace(b.AccessKeyID); v != "" {
		a.AccessKeyID = v
	}
	if v := strings.TrimSpace(b.SecretAccessKey); v != "" {
		a.SecretAccessKey = v
	}
	if v := strings.TrimSpace(b.SecurityToken); v != "" {
		a.SecurityToken = v
	}
	if v := strings.TrimSpace(b.RefreshToken); v != "" {
		a.RefreshToken = v
	}
	if b.ExpiresAt != 0 {
		a.ExpiresAt = b.ExpiresAt
	}
	if v := strings.TrimSpace(b.DomainID); v != "" {
		a.DomainID = v
	}
	if v := strings.TrimSpace(b.UserID); v != "" {
		a.UserID = v
	}
	if v := strings.TrimSpace(b.UserName); v != "" {
		a.UserName = v
	}
	if v := strings.TrimSpace(b.Note); v != "" {
		a.Note = v
	}
	if v := strings.TrimSpace(b.DpopPrivateJwk); v != "" {
		a.DpopPrivateJwk = v
	}
	if v := strings.TrimSpace(b.CodeVerifier); v != "" {
		a.CodeVerifier = v
	}
	// The park switch is a deliberate operator action, so it is ORed: a
	// re-add can never quietly re-enable a credential that was parked.
	a.Disabled = a.Disabled || b.Disabled
	return a
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

// validateSecret rejects values that cannot be a vendor credential.  A pasted
// credential with a newline in it is a copy-paste accident, and letting it
// through produces a signature failure that is far harder to explain than a
// rejected form.
func validateSecret(name, s string, max int) error {
	if s == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(s) > max {
		return fmt.Errorf("%s is longer than %d bytes", name, max)
	}
	for _, r := range s {
		if r == unicode.ReplacementChar || r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains a control character", name)
		}
		if r > 0x7e {
			return fmt.Errorf("%s contains a non-ASCII character", name)
		}
	}
	return nil
}

// validAccountID bounds what the panel may address.
//
// The id is used as a map key in the credential store and appears in the
// panel's URL, so it is restricted to characters that survive both.  A `..`
// sequence is refused as well: the id is never a filename today, and the guard
// costs nothing while removing the possibility that a later change makes it
// one.
func validAccountID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 160 {
		return false
	}
	if strings.Contains(id, "..") {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':', r == '@', r == '/':
		default:
			return false
		}
	}
	return true
}

// sanitizeLabel strips control characters and caps the length so a pasted
// multi-line blob cannot become a panel label.
func sanitizeLabel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= maxLabelRunes*4 {
			break
		}
	}
	out := b.String()
	runes := []rune(out)
	if len(runes) > maxLabelRunes {
		out = string(runes[:maxLabelRunes])
	}
	return strings.TrimSpace(out)
}

// accountFromSpec builds an account out of a panel form submission.
func accountFromSpec(spec core.AccountSpec) (account, error) {
	ak := strings.TrimSpace(spec.Field("access_key_id"))
	sk := strings.TrimSpace(spec.Field("secret_access_key"))
	if err := validateSecret("access_key_id", ak, maxAccessKeyBytes); err != nil {
		return account{}, err
	}
	if err := validateSecret("secret_access_key", sk, maxSecretKeyBytes); err != nil {
		return account{}, err
	}
	a := account{
		ID:              strings.TrimSpace(spec.Field("id")),
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		SecurityToken:   strings.TrimSpace(spec.Field("security_token")),
		RefreshToken:    strings.TrimSpace(spec.Field("refresh_token")),
		ExpiresAt:       parseExpiry(spec.Field("expires_at")),
		DomainID:        strings.TrimSpace(spec.Field("domain_id")),
		UserID:          strings.TrimSpace(spec.Field("user_id")),
		UserName:        strings.TrimSpace(spec.Field("user_name")),
		Note:            sanitizeLabel(firstNonEmpty(spec.Field("label"), spec.Label)),
		Disabled:        !spec.EnabledOr(true),
	}
	if a.ID == "" {
		a.ID = a.accountID()
	}
	if !validAccountID(a.ID) {
		return account{}, fmt.Errorf("account id %q is not usable as a filename-safe id", a.ID)
	}
	return a, nil
}

// firstNonEmpty returns the first trimmed non-empty value.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// firstNonZero returns the first non-zero value.
func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// Where an account came from.  A config-sourced credential is never written to
// the store and cannot be removed from the panel, because the config file is
// its home and a second copy would be free to drift.
const (
	originConfig = "config"
	originStored = "stored"
)

// errNoDataDir is returned by anything that has to persist and cannot.
var errNoDataDir = errors.New("codearts: no data directory is configured, so a credential could not be stored")
