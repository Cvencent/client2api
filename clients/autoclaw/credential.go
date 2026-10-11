package autoclaw

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

// Wire constants for the AutoClaw desktop product.
//
// AutoClaw is an independent platform.  Its API host, credential shape,
// signing identity, account store and model catalogue are its own; nothing
// here is shared with Zhipu, BigModel, ZCode or any other registered client.
const (
	autoclawAPIBase = "https://autoglm-acceleration-api.zhipuai.cn"

	pathModelConfig       = "/autoclaw-proxy/proxy/autoclaw-model-config"
	pathChatCompletions   = "/autoclaw-proxy/proxy/autoclaw/chat/completions"
	pathRefresh           = "/userapi/v1/refresh"
	pathAgentRefresh      = "/userapi/v1/agent-refresh"
	pathUserInfo          = "/userapi/v1/user-info"
	pathUserProfile       = "/userapi/v1/user-profile"
	pathAgentSendCode     = "/userapi/v1/agent-send-code"
	pathAgentLogin        = "/userapi/v1/agent-login"
	pathWalletsV2         = "/agent-assetmgr/api/v2/wallets"
	pathWalletInstancesV1 = "/agent-assetmgr/api/v1/wallet-instances"
	pathLedgers           = "/agent-assetmgr/api/v1/ledgers_std"

	// The desktop client's public product identity.  It is sent on every
	// managed-model request and is not a user secret.
	autoclawAppID   = "100003"
	autoclawAppKey  = "38d2391985e2369a5fb8227d8e6cd5e5"
	autoclawProduct = "autoclaw"

	autoclawVersion  = "2.0.5"
	autoclawPlatform = "win"
	autoclawLanguage = "zh-CN"
	autoclawChannel  = "official"

	tokenRefreshWindow = 300 * time.Second
	requestTimeout     = 60 * time.Second
)

// flexString decodes a JSON string or a bare JSON scalar into a string.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(s, "\"") {
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
func (f flexString) String() string               { return string(f) }

// credential is one AutoClaw account.  Field names match the desktop client's
// decrypted credential document so an imported account round-trips verbatim.
type credential struct {
	AccountID      string     `json:"account_id,omitempty"`
	AccessToken    string     `json:"access_token"`
	RefreshToken   string     `json:"refresh_token,omitempty"`
	ExpiresAt      flexString `json:"expires_at,omitempty"`
	UserID         string     `json:"user_id,omitempty"`
	NumericUserID  string     `json:"numeric_user_id,omitempty"`
	Nickname       string     `json:"nickname,omitempty"`
	Phone          string     `json:"phone,omitempty"`
	DeviceID       string     `json:"device_id,omitempty"`
	OfficeIdentity string     `json:"office_identity,omitempty"`
}

func (c credential) expiresAtMs() (int64, bool) {
	if s := strings.TrimSpace(c.ExpiresAt.String()); s != "" {
		if n, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n > 0 {
			return int64(n), true
		}
	}
	if exp := core.JWTExpiry(c.AccessToken); exp > 0 {
		return exp * 1000, true
	}
	return 0, false
}

func jwtExpiryMs(token string) int64 {
	if exp := core.JWTExpiry(token); exp > 0 {
		return exp * 1000
	}
	return 0
}

func (c credential) expired(now time.Time) bool {
	ms, ok := c.expiresAtMs()
	return ok && ms <= now.UnixMilli()
}

func (c credential) refreshable() bool { return strings.TrimSpace(c.RefreshToken) != "" }

func (c credential) needsRefresh(now time.Time, margin time.Duration) bool {
	ms, ok := c.expiresAtMs()
	return ok && ms-now.UnixMilli() < margin.Milliseconds()
}

func (c credential) identity() string {
	if v := strings.TrimSpace(c.UserID); v != "" {
		return v
	}
	if v := strings.TrimSpace(c.AccountID); v != "" {
		return v
	}
	return core.JWTIdentity(c.AccessToken)
}

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
		return "AutoClaw account"
	}
}

func maskPhone(p string) string {
	p = strings.TrimSpace(p)
	if len(p) <= 4 {
		return p
	}
	return p[:3] + "****" + p[len(p)-4:]
}

func bearerToken(token string) string {
	t := strings.TrimSpace(token)
	if t == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(t), "bearer ") {
		return t
	}
	return "Bearer " + t
}

func autoclawSign(ts string) string {
	sum := md5.Sum([]byte(autoclawAppID + "&" + ts + "&" + autoclawAppKey))
	return hex.EncodeToString(sum[:])
}

// managedIdentityHeaders are the public product headers shared by catalogue,
// chat and account calls.
func managedIdentityHeaders(version string) http.Header {
	h := make(http.Header, 10)
	if strings.TrimSpace(version) == "" {
		version = autoclawVersion
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h.Set("X-Version", version)
	h.Set("X-Tm", autoclawPlatform)
	h.Set("X-Product", autoclawProduct)
	h.Set("X-Auth-Appid", autoclawAppID)
	h.Set("X-Auth-TimeStamp", ts)
	h.Set("X-Auth-Sign", autoclawSign(ts))
	h.Set("X-Lang", autoclawLanguage)
	h.Set("X-Channel", autoclawChannel)
	return h
}

// identityHeaders builds the authenticated flavour used by the model
// catalogue and non-chat account calls.
func identityHeaders(c credential, version string) http.Header {
	h := managedIdentityHeaders(version)
	if tok := bearerToken(c.AccessToken); tok != "" {
		h.Set("Authorization", tok)
	}
	return h
}

// chatHeaders mirrors the desktop managed-model request exactly.
//
// The caller's Authorization is deliberately set last, after deleting both
// spellings, because the desktop resolver builds the product headers first and
// only then injects the credential.
func chatHeaders(c credential, model, sessionID, requestID, version string) http.Header {
	h := managedIdentityHeaders(version)
	h.Del("Authorization")
	h.Del("X-Authorization")
	if tok := bearerToken(c.AccessToken); tok != "" {
		h.Set("X-Authorization", tok)
	}
	if strings.TrimSpace(requestID) == "" {
		requestID = newRequestID()
	}
	h.Set("X-Trace-Id", requestID)
	h.Set("X-Request-Id", requestID)
	h.Set("X-Request-Model", strings.TrimSpace(model))
	h.Set("X-Client-Type", "pc")
	h.Set("x_trace_id", "autoclaw-desktop")
	if s := strings.TrimSpace(sessionID); s != "" {
		h.Set("X-Session-Id", s)
	}
	h.Set("Accept", "text/event-stream")
	h.Set("Content-Type", "application/json")
	return h
}

func newRequestID() string {
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), randomHex(8))
}
