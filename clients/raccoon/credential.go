// Package raccoon exposes 商汤小浣熊 / SenseTime Raccoon Work
// (https://xiaohuanxiong.com) as an OpenAI-compatible chat endpoint.
//
// The wire protocol, the credential shape, the header set, the billing
// multiplier display rules and the phone-transport cipher below were
// transcribed from the vendor's own front-end bundle as recorded in the
// reference implementation (Gitee iJetLi/deepseek-harness-codearts,
// src/raccoon.ts). This Go port is a clean-room re-implementation of that
// behaviour against the public HTTP surface; it shares no code with it.
package raccoon

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

// Wire constants.
const (
	raccoonAPIBase = "https://xiaohuanxiong.com"

	pathAuthPrefix    = "/api/web/auth/v1"
	pathLLMPrefix     = "/api/web/llm/v2"
	pathPointsPrefix  = "/api/web/points/v1"
	pathDesktopPrefix = "/api/web/desktop/v1"

	pathRefresh     = pathAuthPrefix + "/refresh"
	pathUserInfo    = pathAuthPrefix + "/user_info"
	pathQRLogin     = pathAuthPrefix + "/login_with_qrcode_code"
	pathSendSMS     = pathAuthPrefix + "/send_sms"
	pathLoginSMS    = pathAuthPrefix + "/login_with_sms"
	pathCatalog     = pathLLMPrefix + "/model_catalog"
	pathChat        = pathLLMPrefix + "/chat/completions"
	pathBalance     = pathPointsPrefix + "/balance"
	pathBills       = pathPointsPrefix + "/bills"
	pathPointsGrant = pathDesktopPrefix + "/login/points/grant"

	// phoneCipherSecret is a PUBLIC constant that ships inside the vendor's
	// own front-end bundle. It is not a security boundary; it exists only so
	// the SMS path would not put a plaintext phone number on the wire. This
	// module does not implement the SMS path, but the cipher is kept (and
	// pinned by a test) because it is part of the transcribed protocol.
	phoneCipherSecret = "senseraccoon2023"

	// tokenRefreshWindow is how early the pool renews an access token.
	tokenRefreshWindow = 300 * time.Second

	catalogTimeout = 20 * time.Second
	requestTimeout = 60 * time.Second

	clientPlatform = "desktop-windows"
	clientVersion  = "v1.0.35"
	userAgent      = "Raccoon Work/1.0.35 (Windows)"
	vendorLanguage = "zh"
)

// flexString decodes a JSON string OR a bare JSON number/boolean into a
// string. The vendor is inconsistent about quoting numeric fields (for
// example `expires_at` is a millisecond timestamp string in the credential
// store but a bare number in some bundle exports).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = flexString(v)
		return nil
	}
	*f = flexString(s)
	return nil
}

func (f flexString) MarshalJSON() ([]byte, error) { return json.Marshal(string(f)) }

func (f flexString) String() string { return string(f) }

// credential is one raccoon account. The JSON field names are the vendor's
// own credential field names, so a bundle exported from the vendor client
// (or hand-written by an operator) imports verbatim.
type credential struct {
	AccessToken    string     `json:"access_token"`
	RefreshToken   string     `json:"refresh_token,omitempty"`
	ExpiresAt      flexString `json:"expires_at,omitempty"`
	OfficeIdentity string     `json:"office_identity,omitempty"`
	UserID         string     `json:"user_id,omitempty"`
	Nickname       string     `json:"nickname,omitempty"`
	Phone          string     `json:"phone,omitempty"`
	DeviceID       string     `json:"device_id,omitempty"`
}

// expiresAtMs resolves the credential's expiry in unix milliseconds.
//
// `expires_at` is OPTIONAL, so the JWT `exp` is used as a fallback. Reading
// only `expires_at` would make the expiry predicate permanently false and a
// renewal sweep would silently skip the account forever — the same
// silent-failure class as filtering a refresh sweep on `enabled`.
//
// ok is false when the credential carries no expiry information at all.
func (c credential) expiresAtMs() (int64, bool) {
	if s := strings.TrimSpace(c.ExpiresAt.String()); s != "" {
		if n, err := strconv.ParseFloat(s, 64); err == nil &&
			!math.IsNaN(n) && !math.IsInf(n, 0) && n > 0 {
			return int64(n), true
		}
	}
	if exp := core.JWTExpiry(c.AccessToken); exp > 0 {
		return exp * 1000, true
	}
	return 0, false
}

// jwtExpiryMs returns the JWT `exp` claim in unix MILLISECONDS, or 0 when the
// token carries none or cannot be decoded. The signature is never verified:
// this is only used to decide when to renew.
func jwtExpiryMs(token string) int64 {
	if exp := core.JWTExpiry(token); exp > 0 {
		return exp * 1000
	}
	return 0
}

// expired reports whether the access token is known to be past its expiry.
// A credential with NO expiry information is reported as NOT expired: we try
// it and let the server's 401 be the authority.
func (c credential) expired(now time.Time) bool {
	ms, ok := c.expiresAtMs()
	if !ok {
		return false
	}
	return ms <= now.UnixMilli()
}

// refreshable reports whether a renewal is even possible. Raccoon has a real
// refresh endpoint (`POST /api/web/auth/v1/refresh`), so a non-empty refresh
// token is the whole test.
func (c credential) refreshable() bool {
	return strings.TrimSpace(c.RefreshToken) != ""
}

// needsRefresh reports whether the token is inside the proactive renewal
// window. It answers a pure expiry question: whether a renewal is POSSIBLE is
// a separate predicate (`refreshable`), so gating this one on it would make
// the renewal sweep silently skip a credential the moment its refresh token
// is missing. A credential whose expiry is unknown is never refreshed here.
func (c credential) needsRefresh(now time.Time, margin time.Duration) bool {
	ms, ok := c.expiresAtMs()
	if !ok {
		return false
	}
	return ms-now.UnixMilli() < margin.Milliseconds()
}

// identity is the stable grouping key for the account panel: the vendor's
// user id when the credential carries one, otherwise the JWT's subject.
func (c credential) identity() string {
	if id := strings.TrimSpace(c.UserID); id != "" {
		return id
	}
	return core.JWTIdentity(c.AccessToken)
}

// displayName is a human label that is stable across restarts.
func (c credential) displayName() string {
	switch {
	case strings.TrimSpace(c.Nickname) != "" && strings.TrimSpace(c.Phone) != "":
		return c.Nickname + " (" + maskPhone(c.Phone) + ")"
	case strings.TrimSpace(c.Nickname) != "":
		return c.Nickname
	case strings.TrimSpace(c.Phone) != "":
		return maskPhone(c.Phone)
	case c.identity() != "":
		return c.identity()
	default:
		return "raccoon account"
	}
}

func maskPhone(p string) string {
	p = strings.TrimSpace(p)
	if len(p) <= 4 {
		return p
	}
	return p[:3] + "****" + p[len(p)-4:]
}

// raccoonHeaders builds the header set every authenticated request carries.
// `X-Org-Code` is ALWAYS sent (empty for a personal account). The optional
// client headers are only added when non-empty; `X-Client-Platform` is
// required by the desktop points-grant endpoint.
func raccoonHeaders(c credential, platform, version string) http.Header {
	h := make(http.Header, 8)
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+c.AccessToken)
	h.Set("X-Org-Code", c.OfficeIdentity)
	h.Set("X-Raccoon-Language", vendorLanguage)
	if platform != "" {
		h.Set("X-Client-Platform", platform)
	}
	if version != "" {
		h.Set("X-Client-Version", version)
	}
	if strings.TrimSpace(c.DeviceID) != "" {
		h.Set("X-Client-Device-ID", c.DeviceID)
	}
	return h
}

// encryptPhone implements the vendor's phone transport cipher:
// AES-128-CFB with key UTF8("senseraccoon2023"), padding OFF, output
// Base64(iv ‖ ciphertext) with a 16-byte IV.
//
// The IV is a parameter so the behaviour can be pinned against a fixed IV in
// a test.
func encryptPhone(phone string, iv []byte) (string, error) {
	if len(iv) != aes.BlockSize {
		return "", fmt.Errorf("raccoon: iv must be %d bytes, got %d", aes.BlockSize, len(iv))
	}
	key := []byte(phoneCipherSecret)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("raccoon: phone cipher: %w", err)
	}
	out := make([]byte, len(iv)+len(phone))
	copy(out, iv)
	cipher.NewCFBEncrypter(block, iv).XORKeyStream(out[len(iv):], []byte(phone))
	return base64.StdEncoding.EncodeToString(out), nil
}

// encryptPhoneRandom is encryptPhone with a fresh random IV.
func encryptPhoneRandom(phone string) (string, error) {
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", fmt.Errorf("raccoon: phone cipher iv: %w", err)
	}
	return encryptPhone(phone, iv)
}
