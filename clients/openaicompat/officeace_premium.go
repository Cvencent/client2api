package openaicompat

// officeace_premium.go is the signed half of the OfficeAce platform: the model
// gateway's /v1/subscription family, which the desktop client uses for the
// daily bonus claim and for the credit/package breakdown.  It is a distinct
// wire from the OpenAI-compatible chat path -- V11-HMAC-SHA256 rather than an
// HTTP Basic key pair -- so it lives in its own file and never touches the
// transport the chat requests use.
//
// The credential comes from officeace_session.go.  A call that comes back 401
// APIG.0301 refreshes the credential once (OAuth refresh token, then the IAM
// v3 or STS exchange) and retries, mirroring the client's cloudFetch.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	officeAcePremiumPathSubscription       = "/v1/subscription"
	officeAcePremiumPathSubscriptionPeriod = "/v1/subscription/period"
	officeAcePremiumPathBonusClaim         = "/v1/subscription/bonus/claim"
	officeAcePremiumPathProjects           = "/v3/projects"
	officeAcePremiumPathSecurityTok        = "/v3.0/OS-CREDENTIAL/securitytokens"
	officeAcePremiumTokenPath              = "/v1/oauth2/tokens"
	officeAceTrialSKUCode                  = "officeace.personal.package.trial"
	// officeAceSignerSignatureCode is Huawei APIG's "signature is stale" error.
	officeAceSignerStaleCode = "APIG.0301"
	officeAceV3DurationSecs  = 24 * 60 * 60
	officeAceRefreshTTL      = 30 * 24 * time.Hour
)

// officeAcePremium signs the premium routes for one stored account.
type officeAcePremium struct {
	src  *officeAceSource
	row  storedProvider
	sess officeAceStoredSession
}

// officeAceV3Credential is the IAM v3 form of a temporary credential.
type officeAceV3Credential struct {
	Access        string
	Secret        string
	SecurityToken string
	ProjectID     string
	ExpiresAt     string
}

// premiumFor resolves the panel account id to a signed-call helper.  An
// account with no session is a plain error naming the fix, not a transport
// failure: nothing was attempted.
func (o *officeAceSource) premiumFor(id string) (*officeAcePremium, error) {
	if o == nil || o.source == nil {
		return nil, errors.New("officeace: this platform is not available")
	}
	row, ok := o.source.officeAceFindRow(strings.TrimSpace(id))
	if !ok {
		return nil, fmt.Errorf("officeace: account %q not found", id)
	}
	if row.OfficeAce == nil || !row.OfficeAce.usable() {
		return nil, errors.New("officeace: 这个账号还没有云端登录凭据，签到/余额/套餐用不了。请用面板的“华为账号登录”重新添加，或导入本机 OfficeAce 客户端的登录")
	}
	return &officeAcePremium{src: o, row: row, sess: *row.OfficeAce}, nil
}

func (p *officeAcePremium) now() time.Time {
	if p != nil && p.src != nil && p.src.source != nil && p.src.source.now != nil {
		return p.src.source.now()
	}
	return time.Now()
}

func (p *officeAcePremium) save() error {
	return p.src.saveOfficeAceSession(p.row.ID, p.sess)
}

// origin is the model gateway root the premium routes hang off: the account's
// chat root (".../v2") stripped back to its scheme and host.
func (p *officeAcePremium) origin() string {
	return officeAceOrigin(p.row.BaseURL)
}

func officeAceOrigin(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return strings.TrimRight(base, "/")
	}
	return u.Scheme + "://" + u.Host
}

// officeAceRegionFromBaseURL pulls the region out of a model gateway host, so a
// login against another region signs with the right scope.  Anything
// unexpected falls back to the default region.
func officeAceRegionFromBaseURL(base string) string {
	host := ""
	if u, err := url.Parse(strings.TrimSpace(base)); err == nil && u.Host != "" {
		host = u.Hostname()
	} else {
		host = strings.TrimSpace(base)
	}
	labels := strings.Split(strings.ToLower(host), ".")
	for _, label := range labels {
		if strings.HasPrefix(label, "cn-") || strings.HasPrefix(label, "ap-") || strings.HasPrefix(label, "eu-") || strings.HasPrefix(label, "af-") || strings.HasPrefix(label, "la-") || strings.HasPrefix(label, "sa-") {
			if strings.Count(label, "-") >= 1 {
				return label
			}
		}
	}
	return officeAceDefaultRegion
}

// cloudFetch performs one signed premium call and returns the body and status.
// A non-2xx status is data, not an error: callers explain it.  An expired or
// rejected credential is refreshed once and the request is retried.
func (p *officeAcePremium) cloudFetch(ctx context.Context, method, path string, body []byte, headers map[string]string) ([]byte, int, error) {
	return p.cloudFetchBase(ctx, p.origin(), method, path, body, headers)
}

// cloudFetchBase is cloudFetch against an explicit cloud root.  OfficeAce's
// subscription and SKU order endpoints live on the claw service rather than
// the account's model gateway, while the premium summary lives on the gateway.
func (p *officeAcePremium) cloudFetchBase(ctx context.Context, base, method, path string, body []byte, headers map[string]string) ([]byte, int, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil, 0, errors.New("officeace: 云端地址为空")
	}
	endpoint := base + path
	if p.sess.ProjectID == "" {
		p.ensureProject(ctx)
	}
	raw, status, code, err := p.doSigned(ctx, method, endpoint, body, headers)
	if err != nil {
		return nil, 0, err
	}
	if status != http.StatusUnauthorized || code != officeAceSignerStaleCode {
		return raw, status, nil
	}
	if rerr := p.refreshCredentials(ctx); rerr != nil {
		return raw, status, nil
	}
	p.ensureProject(ctx)
	raw, status, _, err = p.doSigned(ctx, method, endpoint, body, headers)
	if err != nil {
		return nil, 0, err
	}
	return raw, status, nil
}

// doSigned signs one request with the current credential and returns the body,
// status and the gateway error_code (when the body carries one).
func (p *officeAcePremium) doSigned(ctx context.Context, method, endpoint string, body []byte, headers map[string]string) ([]byte, int, string, error) {
	merged := map[string]string{}
	for key, value := range headers {
		merged[key] = value
	}
	if !strings.EqualFold(method, http.MethodGet) {
		if _, ok := merged["Content-Type"]; !ok {
			merged["Content-Type"] = "application/json;charset=utf8"
		}
	}
	// The client adds the subscription header case-insensitively; keep it lower
	// so the signed name matches what its signer produced.
	if v, ok := merged["X-Subscription-Type"]; ok {
		delete(merged, "X-Subscription-Type")
		merged["x-subscription-type"] = v
	}
	signed, err := signOfficeAceV11Request(method, endpoint, merged, string(body), p.sess.AccessKey, p.sess.SecretKey, p.sess.SecurityToken, p.sess.ProjectID, p.sess.region(), p.now())
	if err != nil {
		return nil, 0, "", err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var reader io.Reader
	if len(body) > 0 {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(reqCtx, method, endpoint, reader)
	if err != nil {
		return nil, 0, "", fmt.Errorf("officeace: build %s request: %w", method, err)
	}
	for key, value := range signed {
		if strings.EqualFold(key, "host") {
			req.Host = value
			continue
		}
		req.Header.Set(key, value)
	}
	resp, err := p.src.httpClient().Do(req)
	if err != nil {
		return nil, 0, "", fmt.Errorf("officeace: 调用 %s 失败：%w", endpoint, err)
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, maxErrorBytes)
	return raw, resp.StatusCode, officeAceErrorCode(raw), nil
}

// officeAceErrorCode reads error_code from a gateway answer, tolerating the
// data/result envelope.
func officeAceErrorCode(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var doc struct {
		ErrorCode string          `json:"error_code"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	if strings.TrimSpace(doc.ErrorCode) != "" {
		return strings.TrimSpace(doc.ErrorCode)
	}
	if len(doc.Data) > 0 && string(doc.Data) != "null" {
		return officeAceErrorCode(doc.Data)
	}
	return ""
}

// refreshCredentials renews the temporary credential, preferring the durable
// OAuth refresh token and falling back to the IAM v3/STS exchange.  The new
// credential is written to the store immediately: a consumed refresh token is
// invalidated by the cloud, so losing the replacement would strand the
// account.
func (p *officeAcePremium) refreshCredentials(ctx context.Context) error {
	now := p.now()
	if token := strings.TrimSpace(p.sess.RefreshToken); token != "" {
		if err := p.refreshWithToken(ctx); err == nil {
			return p.save()
		}
	}
	if p.sess.v3Usable() && !officeAceExpiresWithin(p.sess.V3ExpiresAt, now, 0) {
		p.applyV3(officeAceV3Credential{
			Access:        p.sess.V3AccessKey,
			Secret:        p.sess.V3SecretKey,
			SecurityToken: p.sess.V3SecurityToken,
			ProjectID:     p.sess.V3ProjectID,
			ExpiresAt:     p.sess.V3ExpiresAt,
		})
		return p.save()
	}
	if v3, ok := officeAceFetchV3(ctx, p.src.httpClient(), p.src.endpoints().KeystoneBase, p.sess.AccessKey, p.sess.SecretKey, p.sess.SecurityToken, p.sess.ProjectID, now); ok {
		p.applyV3(v3)
		return p.save()
	}
	return errors.New("officeace: 云端登录凭据已过期且无法自动续期，请重新登录华为账号")
}

func (p *officeAcePremium) applyV3(v3 officeAceV3Credential) {
	p.sess.AccessKey = strings.TrimSpace(v3.Access)
	p.sess.SecretKey = strings.TrimSpace(v3.Secret)
	p.sess.SecurityToken = strings.TrimSpace(v3.SecurityToken)
	if strings.TrimSpace(v3.ProjectID) != "" {
		p.sess.ProjectID = strings.TrimSpace(v3.ProjectID)
	}
	if strings.TrimSpace(v3.ExpiresAt) != "" {
		p.sess.ExpiresAt = strings.TrimSpace(v3.ExpiresAt)
	}
}

// refreshWithToken exchanges the stored OAuth refresh token for a fresh
// credential, exactly like the client's refreshPrincipal.
func (p *officeAcePremium) refreshWithToken(ctx context.Context) error {
	key := officeAceDecodeScalar(p.sess.DPoPScalar)
	if key == nil {
		return errors.New("officeace: 没有保存 DPoP 私钥，无法自动续期")
	}
	eps := p.src.endpoints()
	tokenURL := strings.TrimRight(eps.IAMBase, "/") + officeAcePremiumTokenPath
	proof, err := officeAceDPoPProof(key, http.MethodPost, tokenURL, p.now())
	if err != nil {
		return err
	}
	form := url.Values{
		"client_id":     {eps.ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {p.sess.RefreshToken},
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("DPoP", proof)
	resp, err := p.src.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, maxErrorBytes)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("officeace: 刷新登录凭据失败：%s", officeAceHTTPError(resp.StatusCode, raw))
	}
	token, err := parseOfficeAceToken(raw)
	if err != nil {
		return err
	}
	p.sess.AccessKey = token.AccessKey
	p.sess.SecretKey = token.SecretKey
	p.sess.SecurityToken = token.SecurityToken
	if strings.TrimSpace(token.ProjectID) != "" {
		p.sess.ProjectID = strings.TrimSpace(token.ProjectID)
	}
	p.sess.ExpiresAt = token.ExpiresAt
	if strings.TrimSpace(token.RefreshToken) != "" {
		p.sess.RefreshToken = strings.TrimSpace(token.RefreshToken)
	}
	p.sess.RefreshExpiresAt = strings.TrimSpace(firstNonEmpty(token.RefreshExpiresAt, p.now().UTC().Add(officeAceRefreshTTL).Format(time.RFC3339)))
	return nil
}

// officeAceFetchV3 mints the IAM v3 credential from a v5 credential.  It is
// best-effort: a failure just means the caller keeps using v5 (or reports a
// stale credential), never a hard login failure.
func officeAceFetchV3(ctx context.Context, client *http.Client, keystoneBase, accessKey, secretKey, securityToken, projectID string, now time.Time) (officeAceV3Credential, bool) {
	keystoneBase = strings.TrimRight(strings.TrimSpace(keystoneBase), "/")
	if keystoneBase == "" || strings.TrimSpace(accessKey) == "" || strings.TrimSpace(secretKey) == "" || strings.TrimSpace(securityToken) == "" {
		return officeAceV3Credential{}, false
	}
	endpoint := keystoneBase + officeAcePremiumPathSecurityTok
	body := fmt.Sprintf(`{"auth":{"identity":{"methods":["token"],"token":{"duration_seconds":%d}}}}`, officeAceV3DurationSecs)
	headers := map[string]string{
		"Content-Type":     "application/json;charset=utf8",
		"X-Security-Token": securityToken,
	}
	signed, err := signOfficeAceRequest(http.MethodPost, endpoint, headers, body, accessKey, secretKey, now)
	if err != nil {
		return officeAceV3Credential{}, false
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return officeAceV3Credential{}, false
	}
	for key, value := range signed {
		if strings.EqualFold(key, "host") {
			req.Host = value
			continue
		}
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return officeAceV3Credential{}, false
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, maxErrorBytes)
	if resp.StatusCode != http.StatusOK {
		return officeAceV3Credential{}, false
	}
	var doc struct {
		Credential struct {
			Access        string `json:"access"`
			Secret        string `json:"secret"`
			SecurityToken string `json:"securitytoken"`
			ExpiresAt     string `json:"expires_at"`
		} `json:"credential"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return officeAceV3Credential{}, false
	}
	v3 := officeAceV3Credential{
		Access:        strings.TrimSpace(doc.Credential.Access),
		Secret:        strings.TrimSpace(doc.Credential.Secret),
		SecurityToken: strings.TrimSpace(doc.Credential.SecurityToken),
		ProjectID:     strings.TrimSpace(projectID),
		ExpiresAt:     strings.TrimSpace(doc.Credential.ExpiresAt),
	}
	if v3.Access == "" || v3.Secret == "" || v3.SecurityToken == "" {
		return officeAceV3Credential{}, false
	}
	return v3, true
}

// ensureProject fills in the project id the premium calls sign with.  It is
// best-effort: the client falls back to an empty id and still tries the call.
func (p *officeAcePremium) ensureProject(ctx context.Context) {
	if strings.TrimSpace(p.sess.ProjectID) != "" {
		return
	}
	eps := p.src.endpoints()
	base := strings.TrimRight(eps.KeystoneBase, "/")
	if base == "" {
		return
	}
	endpoint := base + officeAcePremiumPathProjects
	headers := map[string]string{
		"Accept":           "application/json",
		"X-Security-Token": p.sess.SecurityToken,
	}
	signed, err := signOfficeAceRequest(http.MethodGet, endpoint, headers, "", p.sess.AccessKey, p.sess.SecretKey, p.now())
	if err != nil {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	for key, value := range signed {
		if strings.EqualFold(key, "host") {
			req.Host = value
			continue
		}
		req.Header.Set(key, value)
	}
	resp, err := p.src.httpClient().Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, maxErrorBytes)
	if resp.StatusCode != http.StatusOK {
		return
	}
	var doc struct {
		Projects []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Enabled *bool  `json:"enabled"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return
	}
	region := p.sess.region()
	for _, project := range doc.Projects {
		if project.Enabled != nil && !*project.Enabled {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(project.Name), region) && strings.TrimSpace(project.ID) != "" {
			p.sess.ProjectID = strings.TrimSpace(project.ID)
			_ = p.save()
			return
		}
	}
}

// subscription reads the current subscription, bonus and quota state.
func (p *officeAcePremium) subscription(ctx context.Context) (officeAceSubscription, error) {
	raw, status, err := p.cloudFetch(ctx, http.MethodGet, officeAcePremiumPathSubscription, nil, map[string]string{"x-subscription-type": "v2"})
	if err != nil {
		return officeAceSubscription{}, err
	}
	if status != http.StatusOK {
		return officeAceSubscription{}, fmt.Errorf("officeace: 读取订阅信息失败：%s", officeAceHTTPError(status, raw))
	}
	var sub officeAceSubscription
	if err := json.Unmarshal(raw, &sub); err != nil {
		return officeAceSubscription{}, fmt.Errorf("officeace: 解析订阅信息失败：%w", err)
	}
	return sub, nil
}

// ensureOfficeAceTrial gives a fresh OfficeAce login the same free personal
// trial the desktop client creates after the invitation/subscription step.
// The cloud account can already have model credentials at that point, so this
// runs independently of subscribe() and is idempotent for existing trials.
func (o *officeAceSource) ensureOfficeAceTrial(ctx context.Context, accountID string) error {
	p, err := o.premiumFor(accountID)
	if err != nil {
		return err
	}
	sub, err := p.subscription(ctx)
	if err != nil {
		return err
	}
	if sub.hasTrial() {
		return nil
	}
	atomicProductID, err := p.trialAtomicProductID(ctx)
	if err != nil {
		return err
	}
	return p.createTrialOrder(ctx, atomicProductID)
}

func (s officeAceSubscription) hasTrial() bool {
	for _, sku := range s.Skus {
		if strings.EqualFold(strings.TrimSpace(sku.Code), officeAceTrialSKUCode) {
			return true
		}
	}
	return false
}

func (p *officeAcePremium) trialAtomicProductID(ctx context.Context) (string, error) {
	path := "/v1/skus/" + officeAceTrialSKUCode + "?is_query_sales_cycle=true&is_query_resource_rating=false"
	raw, status, err := p.cloudFetchBase(ctx, p.src.endpoints().ClawBase, http.MethodGet, path, nil, map[string]string{"Accept": "application/json"})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("officeace: 查询个人体验版 SKU 失败：%s", officeAceHTTPError(status, raw))
	}
	var doc struct {
		ProductSalesCycles []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"product_sales_cycles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("officeace: 解析个人体验版 SKU 失败：%w", err)
	}
	for _, cycle := range doc.ProductSalesCycles {
		if id := strings.TrimSpace(cycle.ID); id != "" && (cycle.Status == "" || strings.EqualFold(cycle.Status, "ONSALE")) {
			return id, nil
		}
	}
	return "", errors.New("officeace: 个人体验版没有可购买的销售周期")
}

type officeAceTrialOrder struct {
	ServiceType          string                    `json:"service_type"`
	SubscribePeriodType  int                       `json:"subscribe_period_type"`
	SubscribePeriodValue int                       `json:"subscribe_period_value"`
	AutoRenewal          int                       `json:"auto_renewal"`
	AutoPay              int                       `json:"auto_pay"`
	SubscribeSKUInfo     officeAceTrialSKUInfo     `json:"subscribe_sku_info"`
	AgreementList        []officeAceTrialAgreement `json:"agreement_list"`
}

type officeAceTrialSKUInfo struct {
	SubscribeQuantity        int    `json:"subscribe_quantity"`
	SubscribeSKUCode         string `json:"subscribe_sku_code"`
	SubscribeSubProductCode  string `json:"subscribe_sub_product_code"`
	SubscribeAtomicProductID string `json:"subscribe_atomic_product_id"`
}

type officeAceTrialAgreement struct {
	AgreeDeclaration int    `json:"agree_declaration"`
	Version          string `json:"version"`
	PrivacyStatement string `json:"privacy_statement"`
}

func (p *officeAcePremium) createTrialOrder(ctx context.Context, atomicProductID string) error {
	order := officeAceTrialOrder{
		ServiceType:          "hws.service.type.agentarts",
		SubscribePeriodType:  2,
		SubscribePeriodValue: 1,
		AutoRenewal:          1,
		AutoPay:              1,
		SubscribeSKUInfo: officeAceTrialSKUInfo{
			SubscribeQuantity:        1,
			SubscribeSKUCode:         officeAceTrialSKUCode,
			SubscribeSubProductCode:  "hws.resource.type.agentarts.platform",
			SubscribeAtomicProductID: strings.TrimSpace(atomicProductID),
		},
		AgreementList: []officeAceTrialAgreement{{
			AgreeDeclaration: 1,
			Version:          "1.0",
			PrivacyStatement: "/declaration/agentarts.html",
		}},
	}
	body, err := json.Marshal(order)
	if err != nil {
		return err
	}
	raw, status, err := p.cloudFetchBase(ctx, p.src.endpoints().ClawBase, http.MethodPost, officeAcePremiumPathSubscriptionPeriod, body, map[string]string{"Accept": "application/json"})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("officeace: 创建个人体验版订单失败：%s", officeAceHTTPError(status, raw))
	}
	var result struct {
		Status       string `json:"status"`
		FailedReason string `json:"failed_reason"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("officeace: 解析个人体验版订单结果失败：%w", err)
		}
	}
	if strings.EqualFold(strings.TrimSpace(result.Status), "FAILED") {
		return fmt.Errorf("officeace: 创建个人体验版订单失败：%s", strings.TrimSpace(result.FailedReason))
	}
	return nil
}

// claimBonus posts the daily claim and returns the bonuses the cloud reported
// as newly credited (an empty list means nothing was waiting).
func (p *officeAcePremium) claimBonus(ctx context.Context) ([]officeAceBonus, error) {
	raw, status, err := p.cloudFetch(ctx, http.MethodPost, officeAcePremiumPathBonusClaim, nil, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("officeace: 领取奖励失败：%s", officeAceHTTPError(status, raw))
	}
	var doc struct {
		Bonus []officeAceBonus `json:"bonus_skus"`
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("officeace: 解析领取结果失败：%w", err)
		}
	}
	return doc.Bonus, nil
}

// --- wire shapes ------------------------------------------------------------

type officeAceSubscription struct {
	SubscribeStatus string               `json:"subscribe_status"`
	Skus            []officeAceSku       `json:"skus"`
	Bonus           []officeAceBonus     `json:"bonus_skus"`
	ModelInfos      []officeAceModelInfo `json:"model_infos"`
}

type officeAceSku struct {
	Code       string           `json:"sku_code"`
	Name       string           `json:"sku_name"`
	ResourceID string           `json:"cbc_resource_id"`
	Effective  string           `json:"effective_time"`
	Expired    string           `json:"expired_time"`
	Status     string           `json:"subscribe_status"`
	Quotas     []officeAceQuota `json:"quotas"`
}

type officeAceQuota struct {
	Attr      string `json:"sku_attr_code"`
	Value     string `json:"sku_value"`
	Current   string `json:"current_value"`
	Effective string `json:"effective_time"`
	Expired   string `json:"expired_time"`
}

type officeAceBonus struct {
	ActivityID   string `json:"activity_id"`
	ActivityName string `json:"activity_name"`
	ResourceID   string `json:"cbc_resource_id"`
	Points       string `json:"points"`
	CurrentValue string `json:"current_value"`
	Status       string `json:"status"`
	Effective    string `json:"effective_time"`
	Expired      string `json:"expired_time"`
}

// officeAcePoints is one decoded credit meter.
type officeAcePoints struct {
	Total     int64
	Used      int64
	Unlimited bool
}

func (p officeAcePoints) remaining() int64 {
	if p.Unlimited {
		return 0
	}
	if p.Used >= p.Total {
		return 0
	}
	return p.Total - p.Used
}

// points totals the officeace_points quotas and the still-valid bonus
// activities, exactly the way the client's subscription transformer does.
func (s officeAceSubscription) points(now time.Time) officeAcePoints {
	var total, used int64
	unlimited := false
	for _, sku := range s.Skus {
		for _, quota := range sku.Quotas {
			if quota.Attr != "officeace_points" || !officeAceBonusValid(quota.Expired, now) {
				continue
			}
			value := officeAceInt(quota.Value)
			if value < 0 {
				unlimited = true
				continue
			}
			total += value
			used += officeAceInt(quota.Current)
		}
	}
	for _, bonus := range s.Bonus {
		if !officeAceBonusValid(bonus.Expired, now) {
			continue
		}
		points := officeAceInt(bonus.Points)
		if points < 0 {
			unlimited = true
			continue
		}
		total += points
		used += officeAceInt(bonus.CurrentValue)
	}
	if unlimited {
		return officeAcePoints{Unlimited: true, Used: used}
	}
	return officeAcePoints{Total: total, Used: used}
}

// searchQuota returns the online-search meter when the plan carries one.
func (s officeAceSubscription) searchQuota() (total, used int64, ok bool) {
	for _, sku := range s.Skus {
		for _, quota := range sku.Quotas {
			if quota.Attr != "online_search_count" {
				continue
			}
			return officeAceInt(quota.Value), officeAceInt(quota.Current), true
		}
	}
	return 0, 0, false
}

// planName is the current plan's display name, or empty when there is none.
func (s officeAceSubscription) planName() string {
	if len(s.Skus) == 0 {
		return ""
	}
	return strings.TrimSpace(s.Skus[0].Name)
}

func (s officeAceSubscription) planExpiry() string {
	if len(s.Skus) == 0 {
		return ""
	}
	return strings.TrimSpace(s.Skus[0].Expired)
}

// officeAceBonusValid reports whether an entitlement timestamp is still in the
// future.  An empty or unparseable value counts as valid, which keeps a
// reward without an expiry in the totals.
func officeAceBonusValid(raw string, now time.Time) bool {
	t, ok := officeAceParseTime(raw)
	if !ok {
		return true
	}
	return now.Before(t)
}

func officeAceInt(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	var n int64
	neg := false
	if raw[0] == '-' {
		neg = true
		raw = raw[1:]
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			break
		}
		n = n*10 + int64(raw[i]-'0')
	}
	if neg {
		return -n
	}
	return n
}
