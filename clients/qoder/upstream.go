package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// upstream.go is the HTTP layer: one request builder for the OpenAPI host the
// desktop client uses, plus the four routes this module needs.
//
// Everything here talks to https://openapi.qoder.com.cn.  The vendor's
// inference host (gateway.qoder.com.cn) is deliberately not contacted: it
// requires a per-request signature minted by the desktop client's native
// security SDK, which is not something a Go module can reproduce (see README).

// maxResponseBytes bounds a non-streaming response body.
const maxResponseBytes = 4 << 20

// OpenAPI routes.
const (
	pathUserInfo              = "/api/v1/userinfo"
	pathQuota                 = "/api/v2/quota/usage"
	pathCampaigns             = "/sash/api/v1/me/campaigns"
	pathCampaignLimitedNumber = "/sash/api/v1/me/campaigns/%s/limited-number"
)

type upstream struct {
	cfg  config
	http *http.Client
	logf func(format string, args ...any)
}

func newUpstream(cfg config, shared *http.Client, logf func(format string, args ...any)) *upstream {
	if shared == nil {
		shared = &http.Client{Timeout: cfg.requestTimeout()}
	}
	return &upstream{cfg: cfg, http: shared, logf: logf}
}

func (u *upstream) log(format string, args ...any) {
	if u.logf != nil {
		u.logf(format, args...)
	}
}

// do performs one JSON request against the OpenAPI host.
func (u *upstream) do(ctx context.Context, method, path string, params url.Values, token string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, u.cfg.requestTimeout())
	defer cancel()

	endpoint := u.cfg.openAPIBase() + path
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("qoder: %s %s: %w", method, path, err)
	}
	u.decorate(req, token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qoder: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("qoder: reading %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		code, message := decodeErrorMessage(payload)
		if message == "" {
			message = truncate(strings.TrimSpace(string(payload)), 300)
		}
		return nil, &apiError{Status: resp.StatusCode, Code: code, Message: message}
	}
	return payload, nil
}

// decorate installs the headers every Qoder OpenAPI call carries.  The
// Cosy-* headers are the desktop client's own client-identity headers: the
// endpoints answer a request without them with a 4xx rather than data.
func (u *upstream) decorate(req *http.Request, token string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", u.cfg.userAgent())
	req.Header.Set("Cosy-ClientType", strconv.Itoa(u.cfg.clientType()))
	req.Header.Set("Cosy-Version", u.cfg.cosyVersion())
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// userInfo is the answer to GET /api/v1/userinfo.  It doubles as the credential
// probe: it is cheap, read-only, and 401s exactly when the token is dead.
type userInfo struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Username         string `json:"username"`
	SecurityMobile   string `json:"security_mobile"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
	IsHighestTier    bool   `json:"is_highest_tier"`
}

// quotaBucket is one credit pool.
type quotaBucket struct {
	Total      float64 `json:"total"`
	Used       float64 `json:"used"`
	Remaining  float64 `json:"remaining"`
	Percentage float64 `json:"percentage"`
	Unit       string  `json:"unit"`
}

// quotaUsage is the answer to GET /api/v2/quota/usage.
type quotaUsage struct {
	UserID          string      `json:"userId"`
	UserType        string      `json:"userType"`
	UsageType       string      `json:"usageType"`
	IsQuotaExceeded bool        `json:"isQuotaExceeded"`
	ExpiresAt       int64       `json:"expiresAt"`
	UserQuota       quotaBucket `json:"userQuota"`
	AddOnQuota      quotaBucket `json:"addOnQuota"`
}

// campaign is one entry of the campaigns list.  Only the fields this module
// acts on are modelled; the placements and copy are ignored.
type campaign struct {
	CampaignID  string `json:"campaignId"`
	CampaignKey string `json:"campaignKey"`
	ActionType  string `json:"actionType"`
	StartAt     int64  `json:"startAt"`
	EndAt       int64  `json:"endAt"`
	ClaimStatus string `json:"claimStatus"`
	Benefit     struct {
		Kind   string  `json:"kind"`
		Amount float64 `json:"amount"`
	} `json:"benefit"`
}

// campaignsResponse is the answer to GET /sash/api/v1/me/campaigns.
type campaignsResponse struct {
	UID          string     `json:"uid"`
	ShowCampaign bool       `json:"showCampaign"`
	Claimable    bool       `json:"claimable"`
	CampaignURL  string     `json:"campaignUrl"`
	Campaigns    []campaign `json:"campaigns"`
}

// ---------------------------------------------------------------------------
// Calls
// ---------------------------------------------------------------------------

// userInfo reads the signed-in identity.
func (u *upstream) userInfo(ctx context.Context, token string) (*userInfo, error) {
	payload, err := u.do(ctx, http.MethodGet, pathUserInfo, nil, token, nil)
	if err != nil {
		return nil, err
	}
	var info userInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		return nil, fmt.Errorf("qoder: reading userinfo: %w", err)
	}
	return &info, nil
}

// quotaUsage reads the credit ledger.
func (u *upstream) quotaUsage(ctx context.Context, token string) (*quotaUsage, error) {
	payload, err := u.do(ctx, http.MethodGet, pathQuota, nil, token, nil)
	if err != nil {
		return nil, err
	}
	var usage quotaUsage
	if err := json.Unmarshal(payload, &usage); err != nil {
		return nil, fmt.Errorf("qoder: reading the quota ledger: %w", err)
	}
	return &usage, nil
}

// campaigns lists the vendor's running activities for one account.  This is the
// read-only half of the daily check-in.
func (u *upstream) campaigns(ctx context.Context, token string) (*campaignsResponse, error) {
	payload, err := u.do(ctx, http.MethodGet, pathCampaigns, nil, token, nil)
	if err != nil {
		return nil, err
	}
	var out campaignsResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("qoder: reading the campaign list: %w", err)
	}
	return &out, nil
}

// claimCampaign performs the write half: POST one campaign's claim route.
//
// The vendor answers with the campaign's new status, either at the top level or
// under a `data` object, so both spellings are accepted.
func (u *upstream) claimCampaign(ctx context.Context, token, campaignID string) (string, error) {
	path := pathCampaigns + "/" + url.PathEscape(campaignID) + "/claim"
	payload, err := u.do(ctx, http.MethodPost, path, nil, token, nil)
	if err != nil {
		return "", err
	}
	return claimStatus(payload), nil
}

// claimStatus reads the post-claim status out of either envelope shape.
func claimStatus(payload []byte) string {
	var fields map[string]json.RawMessage
	if len(payload) == 0 || json.Unmarshal(payload, &fields) != nil {
		return ""
	}
	if status := scalarString(fields["status"]); status != "" {
		return status
	}
	if raw, ok := fields["data"]; ok {
		var inner map[string]json.RawMessage
		if json.Unmarshal(raw, &inner) == nil {
			return scalarString(inner["status"])
		}
	}
	return ""
}

func scalarString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

// limitedNumber is the answer to the launch campaign's queue-number route.  It
// is informational only: the module never draws a number on the operator's
// behalf.
type limitedNumber struct {
	HasNumber bool   `json:"hasNumber"`
	Number    int64  `json:"number"`
	CreatedAt string `json:"createdAt"`
}

func (u *upstream) limitedNumber(ctx context.Context, token, campaignKey string) (*limitedNumber, error) {
	path := fmt.Sprintf(pathCampaignLimitedNumber, url.PathEscape(campaignKey))
	payload, err := u.do(ctx, http.MethodGet, path, nil, token, nil)
	if err != nil {
		return nil, err
	}
	var out limitedNumber
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("qoder: reading the limited number: %w", err)
	}
	return &out, nil
}
