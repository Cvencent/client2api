package openaicompat

// officeace_session.go keeps the Huawei cloud credential one OfficeAce login
// mints.  The model gateway only needs the model_app_key/model_app_secret pair
// for chat, but the premium routes the desktop client serves -- the daily
// bonus claim, the subscription/credit breakdown, the usage meters -- are
// signed with the temporary access key the OAuth login returned.  Persisting
// that credential next to the account lets this platform offer the same
// features the client does.
//
// Two credentials arrive from the cloud and the client keeps both:
//
//   - the V11 credential, used with V11-HMAC-SHA256 against the model gateway;
//   - the V3 credential, used with SDK-HMAC-SHA256 against the cloud IAM.
//
// A refresh token (valid about a month) can mint replacements once the
// temporary credentials expire, so a login stays useful without the desktop
// client running.

import (
	"crypto/ecdsa"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// officeAceDefaultRegion is the region the desktop client defaults to and
	// the only one OfficeAce has shipped against so far.
	officeAceDefaultRegion = "cn-southwest-2"
	// officeAceIAMHost serves both the project list and the STS endpoint the
	// premium flow uses to mint a fresh temporary credential.
	officeAceIAMHost = "https://iam.myhuaweicloud.com"
)

// officeAceStoredSession is the on-disk shape of one OfficeAce cloud login.
// Every secret here is as sensitive as the model key pair beside it; the panel
// never renders it, and the JSON store is the same accounts.json file.
type officeAceStoredSession struct {
	Region string `json:"region,omitempty"`

	// V11 / model-gateway credential.
	AccessKey     string `json:"access_key,omitempty"`
	SecretKey     string `json:"secret_key,omitempty"`
	SecurityToken string `json:"security_token,omitempty"`
	ProjectID     string `json:"project_id,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`

	// V3 / IAM credential (SDK-HMAC-SHA256).
	V3AccessKey     string `json:"v3_access_key,omitempty"`
	V3SecretKey     string `json:"v3_secret_key,omitempty"`
	V3SecurityToken string `json:"v3_security_token,omitempty"`
	V3ProjectID     string `json:"v3_project_id,omitempty"`
	V3ExpiresAt     string `json:"v3_expires_at,omitempty"`

	// OAuth refresh material.  DPoPScalar is the raw P-256 private scalar the
	// login generated, kept so a refresh can mint a fresh DPoP proof.
	RefreshToken     string `json:"refresh_token,omitempty"`
	RefreshExpiresAt string `json:"refresh_expires_at,omitempty"`
	DPoPScalar       string `json:"dpop_scalar,omitempty"`

	AccountID string `json:"account_id,omitempty"`
	UserName  string `json:"user_name,omitempty"`
}

// usable reports whether the session can sign a premium call right now (or
// refresh itself into one).
func (s officeAceStoredSession) usable() bool {
	if strings.TrimSpace(s.AccessKey) != "" && strings.TrimSpace(s.SecretKey) != "" && strings.TrimSpace(s.SecurityToken) != "" {
		return true
	}
	return strings.TrimSpace(s.RefreshToken) != "" || strings.TrimSpace(s.V3AccessKey) != ""
}

func (s officeAceStoredSession) region() string {
	if r := strings.TrimSpace(s.Region); r != "" {
		return r
	}
	return officeAceDefaultRegion
}

// v3Usable reports whether the IAM credential can be used as-is.
func (s officeAceStoredSession) v3Usable() bool {
	return strings.TrimSpace(s.V3AccessKey) != "" && strings.TrimSpace(s.V3SecretKey) != "" && strings.TrimSpace(s.V3SecurityToken) != ""
}

// officeAceSessionFromLogin builds the stored session from a completed OAuth
// login.
func officeAceSessionFromLogin(token officeAceToken, dpop *ecdsa.PrivateKey, identity officeAceIdentity, region string) officeAceStoredSession {
	sess := officeAceStoredSession{
		Region:           firstNonEmpty(strings.TrimSpace(region), officeAceDefaultRegion),
		AccessKey:        strings.TrimSpace(token.AccessKey),
		SecretKey:        strings.TrimSpace(token.SecretKey),
		SecurityToken:    strings.TrimSpace(token.SecurityToken),
		ProjectID:        strings.TrimSpace(token.ProjectID),
		ExpiresAt:        strings.TrimSpace(token.ExpiresAt),
		RefreshToken:     strings.TrimSpace(token.RefreshToken),
		RefreshExpiresAt: strings.TrimSpace(token.RefreshExpiresAt),
		AccountID:        strings.TrimSpace(identity.AccountID),
		UserName:         firstNonEmpty(strings.TrimSpace(identity.UserName), strings.TrimSpace(token.UserName)),
	}
	sess.V3AccessKey = strings.TrimSpace(token.V3AccessKey)
	sess.V3SecretKey = strings.TrimSpace(token.V3SecretKey)
	sess.V3SecurityToken = strings.TrimSpace(token.V3SecurityToken)
	sess.V3ProjectID = strings.TrimSpace(token.V3ProjectID)
	sess.V3ExpiresAt = strings.TrimSpace(token.V3ExpiresAt)
	if dpop != nil {
		sess.DPoPScalar = officeAceEncodeScalar(dpop)
	}
	return sess
}

func officeAceEncodeScalar(key *ecdsa.PrivateKey) string {
	if key == nil || key.D == nil {
		return ""
	}
	buf := make([]byte, 32)
	return base64.RawURLEncoding.EncodeToString(key.D.FillBytes(buf))
}

// officeAceDecodeScalar reconstructs the DPoP signing key from its stored
// scalar.  An absent or malformed scalar yields a nil key, which the refresh
// path reports as "no DPoP key" rather than failing hard.
func officeAceDecodeScalar(raw string) *ecdsa.PrivateKey {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	scalar, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(raw, "="))
	if err != nil || len(scalar) == 0 {
		return nil
	}
	priv, err := officeAcePrivateKeyFromScalar(scalar)
	if err != nil {
		return nil
	}
	return priv
}

// officeAceParseTime accepts the several timestamp shapes the cloud returns,
// including one without a zone that the desktop client treats as UTC.
func officeAceParseTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// officeAceExpiresWithin reports whether raw names a time at or before the
// given moment.  An unparseable timestamp is treated as "not expired": the
// cloud rejects a stale credential and the caller refreshes on APIG.0301 or
// APIG.0602.
func officeAceExpiresWithin(raw string, now time.Time, margin time.Duration) bool {
	t, ok := officeAceParseTime(raw)
	if !ok {
		return false
	}
	return !now.Add(margin).Before(t)
}

// --- source helpers ---------------------------------------------------------

// officeAceFindRow returns a copy of the stored row for one account id.
func (s *source) officeAceFindRow(id string) (storedProvider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.rows {
		if row.ID == id {
			return row, true
		}
	}
	return storedProvider{}, false
}

// officeAceHasPremiumAccount reports whether any stored account carries a
// session that can sign a premium call.  It is what gates the check-in action
// list, so a pool of import-only accounts offers no button that could only
// fail.
func (o *officeAceSource) officeAceHasPremiumAccount() bool {
	if o == nil || o.source == nil {
		return false
	}
	o.source.mu.Lock()
	defer o.source.mu.Unlock()
	for _, row := range o.source.rows {
		if row.OfficeAce != nil && row.OfficeAce.usable() && !row.Disabled {
			return true
		}
	}
	return false
}

// saveOfficeAceSession writes a refreshed session back onto one account row.
// Nothing else on the row changes, so an in-flight chat request keeps the same
// model key.
func (o *officeAceSource) saveOfficeAceSession(id string, sess officeAceStoredSession) error {
	if o == nil || o.source == nil {
		return errors.New("officeace: this platform is not available")
	}
	s := o.source
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := append([]storedProvider(nil), s.rows...)
	for i := range rows {
		if rows[i].ID != id {
			continue
		}
		copy := sess
		rows[i].OfficeAce = &copy
		return s.saveLocked(rows)
	}
	return fmt.Errorf("officeace: account %q not found", id)
}
