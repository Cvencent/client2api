package openaicompat

// officeace_login.go runs the Huawei account sign-in the OfficeAce desktop
// client itself uses, so the platform can mint model-gateway credentials
// without the desktop client being installed on the machine at all.
//
// The flow, mirrored from the client's own auth/huawei-oauth.js:
//
//  1. POST {claw}/v1/claw/auth/state                 -> {state}
//  2. the operator opens a logout-wrapped {auth}/authui/login.html?service=...
//     in a browser, which clears any earlier Huawei web session first
//  3. GET  {claw}/v1/claw/auth/code?state=...        -> {code} once approved
//  4. POST {iam}/v1/oauth2/tokens                    -> IAM temporary credential
//  5. GET  {claw}/v1/claw/client-permission-validate -> identity + model_info
//  6. POST {claw}/v1/claw/client-subscription        -> model_info for a login
//     that has not subscribed yet
//  7. POST {claw}/v1/subscription/period             -> free 个人体验版
//     entitlement (500 credits and 200 searches)
//
// Steps 5 and 6 are signed with the temporary access key/secret
// (SDK-HMAC-SHA256, see officeace_sign.go); the model_info they return carries
// model_api_url_base plus the model_app_key / model_app_secret pair this
// platform authenticates with.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

const (
	// The OAuth client the desktop app is registered as, and the endpoints it
	// talks to.  They are constants in the client too.
	officeAceOAuthClientID = "pdp5_for_agentarts"
	officeAceClawBase      = "https://agentarts.cn-southwest-2.myhuaweicloud.com"
	officeAceAuthBase      = "https://auth.huaweicloud.com"
	officeAceIAMBase       = "https://sts.cn-north-4.myhuaweicloud.com"
	// officeAceKeystoneBase serves the project list and the STS security-token
	// exchange the premium routes need, which is a different host from the
	// OAuth token endpoint above.
	officeAceKeystoneBase = "https://iam.myhuaweicloud.com"
	officeAceRedirectPath = "/v1/claw/auth/callback"

	// officeAceLoginTTL bounds one interactive sign-in.  The cloud expires its
	// state after ten minutes, so a session that outlives that can never
	// succeed and is reported as timed out instead of polling forever.
	officeAceLoginTTL = 10 * time.Minute
	// officeAceExchangeTimeout bounds the code exchange plus the two signed
	// calls that follow it, which all happen inside one panel poll.
	officeAceExchangeTimeout = 45 * time.Second
	// officeAceBannedErrorCode is the cloud's "this account is banned" answer.
	officeAceBannedErrorCode = "AgentArts.11000006"
)

// officeAceEndpoints is the endpoint set one sign-in talks to.  It is a struct
// rather than constants so tests can point the whole flow at their own server.
type officeAceEndpoints struct {
	ClawBase     string
	AuthBase     string
	IAMBase      string
	KeystoneBase string
	ClientID     string
	RedirectURI  string
}

func defaultOfficeAceEndpoints() officeAceEndpoints {
	return officeAceEndpoints{
		ClawBase:     officeAceClawBase,
		AuthBase:     officeAceAuthBase,
		IAMBase:      officeAceIAMBase,
		KeystoneBase: officeAceKeystoneBase,
		ClientID:     officeAceOAuthClientID,
		RedirectURI:  officeAceClawBase + officeAceRedirectPath,
	}
}

// endpoints resolves the sign-in endpoints, defaulting a zero value to the
// shipped set so a directly constructed source still works.
func (o *officeAceSource) endpoints() officeAceEndpoints {
	eps := o.eps
	def := defaultOfficeAceEndpoints()
	if strings.TrimSpace(eps.ClawBase) == "" {
		eps.ClawBase = def.ClawBase
	}
	if strings.TrimSpace(eps.AuthBase) == "" {
		eps.AuthBase = def.AuthBase
	}
	if strings.TrimSpace(eps.IAMBase) == "" {
		eps.IAMBase = def.IAMBase
	}
	if strings.TrimSpace(eps.KeystoneBase) == "" {
		eps.KeystoneBase = def.KeystoneBase
	}
	if strings.TrimSpace(eps.ClientID) == "" {
		eps.ClientID = def.ClientID
	}
	if strings.TrimSpace(eps.RedirectURI) == "" {
		eps.RedirectURI = strings.TrimRight(eps.ClawBase, "/") + officeAceRedirectPath
	}
	return eps
}

// officeAceLoginSession is one in-flight sign-in.  The state doubles as the
// session id: it is what the cloud ties the authorisation code to, so there is
// nothing to correlate by hand.
type officeAceLoginSession struct {
	id        string
	url       string
	state     string
	verifier  string
	key       *ecdsa.PrivateKey
	startedAt time.Time

	mu        sync.Mutex
	status    string
	message   string
	accountID string
}

func (s *officeAceLoginSession) snapshot() core.LoginState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return core.LoginState{
		SessionID: s.id,
		State:     s.status,
		URL:       s.url,
		Message:   s.message,
		AccountID: s.accountID,
	}
}

func (s *officeAceLoginSession) finished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status != core.LoginPending
}

// setMessage updates the pending line the panel shows while it polls.  A
// finished session is never rewritten: the first terminal state wins.
func (s *officeAceLoginSession) setMessage(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == core.LoginPending {
		s.message = msg
	}
}

func (s *officeAceLoginSession) finish(status, message, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != core.LoginPending {
		return
	}
	s.status = status
	s.message = message
	if accountID != "" {
		s.accountID = accountID
	}
}

var _ core.LoginProvider = (*officeAceSource)(nil)

// StartLogin asks the cloud for an authorisation state and returns the Huawei
// sign-in URL for it.  Nothing secret leaves the machine in this step: the
// request carries a PKCE challenge and the DPoP key is generated locally.
func (o *officeAceSource) StartLogin(ctx context.Context) (core.LoginState, error) {
	if o == nil || o.source == nil {
		return core.LoginState{}, errors.New("officeace: this platform is not available")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	eps := o.endpoints()
	state, err := o.requestLoginState(ctx, eps)
	if err != nil {
		return core.LoginState{}, err
	}
	verifier, err := officeAceRandomToken(32)
	if err != nil {
		return core.LoginState{}, fmt.Errorf("officeace: no entropy for the sign-in: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return core.LoginState{}, fmt.Errorf("officeace: no entropy for the sign-in key: %w", err)
	}
	s := &officeAceLoginSession{
		id:        state,
		state:     state,
		verifier:  verifier,
		key:       key,
		startedAt: o.source.now(),
		status:    core.LoginPending,
		url:       officeAceAuthorizeURL(eps, state, officeAcePKCEChallenge(verifier)),
	}
	o.putLogin(s)
	o.source.deps.Log("%s: Huawei sign-in started", officeAceSourceID)
	return s.snapshot(), nil
}

// CancelLogin drops a session.  It is idempotent: cancelling a session that
// already finished, or never existed, is not an error.
func (o *officeAceSource) CancelLogin(ctx context.Context, sessionID string) error {
	if o == nil {
		return nil
	}
	o.loginMu.Lock()
	delete(o.logins, strings.TrimSpace(sessionID))
	o.loginMu.Unlock()
	return nil
}

// --- session store ----------------------------------------------------------

func (o *officeAceSource) putLogin(s *officeAceLoginSession) {
	o.loginMu.Lock()
	defer o.loginMu.Unlock()
	if o.logins == nil {
		o.logins = map[string]*officeAceLoginSession{}
	}
	now := o.source.now()
	for id, old := range o.logins {
		if now.Sub(old.startedAt) > officeAceLoginTTL {
			delete(o.logins, id)
		}
	}
	o.logins[s.id] = s
}

func (o *officeAceSource) getLogin(id string) (*officeAceLoginSession, bool) {
	o.loginMu.Lock()
	defer o.loginMu.Unlock()
	s, ok := o.logins[id]
	return s, ok
}

// PollLogin advances one session by at most one step: it asks the cloud for
// the authorisation code and, once there is one, exchanges it and stores the
// account.  The panel owns the polling cadence.
func (o *officeAceSource) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	if o == nil || o.source == nil {
		return core.LoginState{}, errors.New("officeace: this platform is not available")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	session, ok := o.getLogin(strings.TrimSpace(sessionID))
	if !ok {
		return core.LoginState{}, fmt.Errorf("officeace: no sign-in session %q", sessionID)
	}
	if session.finished() {
		return session.snapshot(), nil
	}
	if o.source.now().Sub(session.startedAt) > officeAceLoginTTL {
		session.finish(core.LoginFailed, "登录超时，请重新发起授权", "")
		return session.snapshot(), nil
	}
	eps := o.endpoints()
	result, err := o.pollAuthorisationCode(ctx, eps, session.state)
	if err != nil {
		// A transport hiccup while the operator is still on the Huawei page is
		// not a failed login: keep the session pending and say what happened.
		session.setMessage("等待华为账号授权…（" + truncate(core.Redact(err.Error()), 160) + "）")
		return session.snapshot(), nil
	}
	if result.Error != "" {
		session.finish(core.LoginFailed, "华为账号授权未通过："+truncate(core.Redact(result.Error), 240), "")
		return session.snapshot(), nil
	}
	if result.Code == "" {
		session.setMessage("等待华为账号授权…在浏览器里完成登录后会自动继续")
		return session.snapshot(), nil
	}
	if err := o.completeLogin(ctx, session, result.Code); err != nil {
		session.finish(core.LoginFailed, truncate(core.Redact(err.Error()), 300), "")
	}
	return session.snapshot(), nil
}

// --- cloud calls ------------------------------------------------------------

func (o *officeAceSource) httpClient() *http.Client {
	if o.source != nil && o.source.deps.HTTPClient != nil {
		return o.source.deps.HTTPClient
	}
	return defaultHTTPClient
}

// requestLoginState asks the model-gateway service for the state this sign-in
// will be correlated by.  The desktop client stopped generating it locally
// when the cloud started rejecting unknown states.
func (o *officeAceSource) requestLoginState(ctx context.Context, eps officeAceEndpoints) (string, error) {
	endpoint := strings.TrimRight(eps.ClawBase, "/") + "/v1/claw/auth/state"
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		return "", fmt.Errorf("officeace: build state request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("officeace: 获取登录 state 失败：%w", err)
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, maxErrorBytes)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("officeace: 获取登录 state 失败：%s", officeAceHTTPError(resp.StatusCode, raw))
	}
	var payload struct {
		State string `json:"state"`
		Data  struct {
			State string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("officeace: 解析登录 state 失败：%w", err)
	}
	state := firstNonEmpty(payload.State, payload.Data.State)
	if state == "" {
		return "", errors.New("officeace: 云端没有返回登录 state")
	}
	return state, nil
}

// officeAceCodeResult is what one poll of the authorisation endpoint said.
type officeAceCodeResult struct {
	Code  string
	Error string
}

// pollAuthorisationCode asks the cloud whether the operator finished.  An
// empty result means "not yet"; the endpoint answers with the code once the
// Huawei page redirects to the gateway's callback.
func (o *officeAceSource) pollAuthorisationCode(ctx context.Context, eps officeAceEndpoints, state string) (officeAceCodeResult, error) {
	endpoint := strings.TrimRight(eps.ClawBase, "/") + "/v1/claw/auth/code?state=" + url.QueryEscape(state)
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return officeAceCodeResult{}, fmt.Errorf("officeace: build code request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient().Do(req)
	if err != nil {
		return officeAceCodeResult{}, fmt.Errorf("officeace: 查询授权结果失败：%w", err)
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, maxErrorBytes)
	// The desktop client treats 202 as "authorisation still pending"; 204 and
	// 404 are tolerated as the same answer by older gateway builds.
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return officeAceCodeResult{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return officeAceCodeResult{}, fmt.Errorf("officeace: 查询授权结果失败：%s", officeAceHTTPError(resp.StatusCode, raw))
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return officeAceCodeResult{}, nil
	}
	var payload struct {
		Code             string `json:"code"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Data             struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return officeAceCodeResult{}, fmt.Errorf("officeace: 解析授权结果失败：%w", err)
	}
	if strings.TrimSpace(payload.Error) != "" {
		return officeAceCodeResult{Error: firstNonEmpty(payload.ErrorDescription, payload.Error)}, nil
	}
	return officeAceCodeResult{Code: firstNonEmpty(payload.Code, payload.Data.Code)}, nil
}

// officeAceToken is the temporary IAM credential the token endpoint issues.
type officeAceToken struct {
	AccessKey     string
	SecretKey     string
	SecurityToken string
	ProjectID     string
	ExpiresAt     string
	RefreshToken  string
	// RefreshExpiresAt is present only when the cloud said so; the login stores
	// an estimate otherwise.
	RefreshExpiresAt string
	Subject          string
	UserName         string

	// The IAM v3 credential is minted separately (see fetchOfficeAceV3Credential)
	// and kept on the token so one code path can persist both credentials.
	V3AccessKey     string
	V3SecretKey     string
	V3SecurityToken string
	V3ProjectID     string
	V3ExpiresAt     string
}

// exchangeAuthorizationCode trades the code for the temporary AK/SK the
// signed gateway calls need.  The token request carries a fresh DPoP proof,
// exactly like the desktop client's.
func (o *officeAceSource) exchangeAuthorizationCode(ctx context.Context, eps officeAceEndpoints, session *officeAceLoginSession, code string) (officeAceToken, error) {
	tokenURL := strings.TrimRight(eps.IAMBase, "/") + "/v1/oauth2/tokens"
	form := url.Values{
		"client_id":     {eps.ClientID},
		"code":          {code},
		"code_verifier": {session.verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {eps.RedirectURI},
	}
	proof, err := officeAceDPoPProof(session.key, http.MethodPost, tokenURL, time.Now())
	if err != nil {
		return officeAceToken{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return officeAceToken{}, fmt.Errorf("officeace: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("DPoP", proof)
	resp, err := o.httpClient().Do(req)
	if err != nil {
		return officeAceToken{}, fmt.Errorf("officeace: 授权码兑换失败：%w", err)
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, maxErrorBytes)
	if resp.StatusCode != http.StatusOK {
		return officeAceToken{}, fmt.Errorf("officeace: 授权码兑换失败：%s", officeAceHTTPError(resp.StatusCode, raw))
	}
	return parseOfficeAceToken(raw)
}

func parseOfficeAceToken(raw []byte) (officeAceToken, error) {
	type credential struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SecurityToken   string `json:"security_token"`
		ProjectID       string `json:"project_id"`
		Expiration      string `json:"expiration"`
	}
	var doc struct {
		Credentials      credential `json:"credentials"`
		IDToken          string     `json:"id_token"`
		RefreshToken     string     `json:"refresh_token"`
		RefreshExpiresAt string     `json:"refresh_expires_at"`
		Data             struct {
			Credentials      credential `json:"credentials"`
			IDToken          string     `json:"id_token"`
			RefreshToken     string     `json:"refresh_token"`
			RefreshExpiresAt string     `json:"refresh_expires_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return officeAceToken{}, fmt.Errorf("officeace: 解析授权结果失败：%w", err)
	}
	cred := doc.Credentials
	idToken := doc.IDToken
	refresh := doc.RefreshToken
	if strings.TrimSpace(cred.AccessKeyID) == "" && strings.TrimSpace(doc.Data.Credentials.AccessKeyID) != "" {
		cred = doc.Data.Credentials
		idToken = firstNonEmpty(idToken, doc.Data.IDToken)
		refresh = firstNonEmpty(refresh, doc.Data.RefreshToken)
	}
	tok := officeAceToken{
		AccessKey:        strings.TrimSpace(cred.AccessKeyID),
		SecretKey:        strings.TrimSpace(cred.SecretAccessKey),
		SecurityToken:    strings.TrimSpace(cred.SecurityToken),
		ProjectID:        strings.TrimSpace(cred.ProjectID),
		ExpiresAt:        strings.TrimSpace(cred.Expiration),
		RefreshToken:     strings.TrimSpace(refresh),
		RefreshExpiresAt: strings.TrimSpace(firstNonEmpty(doc.RefreshExpiresAt, doc.Data.RefreshExpiresAt)),
	}
	if tok.AccessKey == "" || tok.SecretKey == "" || tok.SecurityToken == "" {
		return officeAceToken{}, errors.New("officeace: 授权结果缺少临时访问凭据")
	}
	if claims := officeAceIDTokenClaims(idToken); len(claims) > 0 {
		tok.Subject = claims["sub"]
		tok.UserName = firstNonEmpty(claims["preferred_username"], claims["name"])
	}
	return tok, nil
}

// officeAceIDTokenClaims decodes the (already transport-protected) id_token
// payload for the subject and display name.  A token that cannot be decoded
// simply contributes nothing.
func officeAceIDTokenClaims(idToken string) map[string]string {
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	out := make(map[string]string, 3)
	for _, key := range []string{"sub", "preferred_username", "name"} {
		if v, ok := doc[key].(string); ok && strings.TrimSpace(v) != "" {
			out[key] = strings.TrimSpace(v)
		}
	}
	return out
}

// officeAceIdentity is who the cloud says this login belongs to.
type officeAceIdentity struct {
	AccountID    string
	PrincipalID  string
	PrincipalURN string
	UserName     string
}

// key is the identity spelled the way the desktop client names its stored
// profiles (accountId:principalId), so a later local import of the same login
// lands on the same account row.
func (i officeAceIdentity) key() string {
	account := strings.TrimSpace(i.AccountID)
	principal := strings.TrimSpace(i.PrincipalID)
	switch {
	case account != "" && principal != "":
		return account + ":" + principal
	case account != "":
		return account
	default:
		return principal
	}
}

// officeAceModelInfo is the model-gateway block the cloud hands out at the end
// of a login: the OpenAI-compatible root plus the application key pair.
type officeAceModelInfo struct {
	APIURLBase string `json:"model_api_url_base"`
	AuthInfo   struct {
		AppKey    string `json:"model_app_key"`
		AppSecret string `json:"model_app_secret"`
	} `json:"model_auth_info"`
}

func (m officeAceModelInfo) usable() bool {
	return strings.TrimSpace(m.AuthInfo.AppKey) != "" && strings.TrimSpace(m.AuthInfo.AppSecret) != ""
}

// officeAcePermissionDocument mirrors the permission/subscription envelope the
// gateway answers with.  The interesting fields may sit at the top level or
// under "data"/"result", and the model block may be nested under
// "subscription", so the accessors below flatten both shapes.
type officeAcePermissionDocument struct {
	AccountID    string              `json:"account_id"`
	PrincipalID  string              `json:"principal_id"`
	PrincipalURN string              `json:"principal_urn"`
	ModelInfo    *officeAceModelInfo `json:"model_info"`

	Data   *officeAcePermissionDocument `json:"data"`
	Result *officeAcePermissionDocument `json:"result"`

	Subscription *struct {
		ModelInfo *officeAceModelInfo          `json:"model_info"`
		Data      *officeAcePermissionDocument `json:"data"`
		Result    *officeAcePermissionDocument `json:"result"`
	} `json:"subscription"`

	ErrorCode       string `json:"error_code"`
	ErrorMsg        string `json:"error_msg"`
	ErrorMessage    string `json:"error_message"`
	ErrorReason     string `json:"error_reason"`
	ErrorSuggestion string `json:"error_suggestion"`
}

// payload returns the node that actually carries the answer, unwrapping the
// "data"/"result" envelope the service wraps its answers in.
func (d *officeAcePermissionDocument) payload() *officeAcePermissionDocument {
	if d == nil {
		return nil
	}
	if d.Data != nil {
		return d.Data.payload()
	}
	if d.Result != nil {
		return d.Result.payload()
	}
	return d
}

// model flattens the model_info block, including the "subscription" wrapper
// the subscription endpoint answers with.
func (d *officeAcePermissionDocument) model() officeAceModelInfo {
	if d == nil {
		return officeAceModelInfo{}
	}
	if payload := d.payload(); payload != d {
		return payload.model()
	}
	if d.ModelInfo != nil {
		return *d.ModelInfo
	}
	if sub := d.Subscription; sub != nil {
		if sub.ModelInfo != nil {
			return *sub.ModelInfo
		}
		if sub.Data != nil {
			return sub.Data.payload().model()
		}
		if sub.Result != nil {
			return sub.Result.payload().model()
		}
	}
	return officeAceModelInfo{}
}

func (d *officeAcePermissionDocument) identity() officeAceIdentity {
	payload := d.payload()
	if payload == nil {
		return officeAceIdentity{}
	}
	return officeAceIdentity{
		AccountID:    strings.TrimSpace(payload.AccountID),
		PrincipalID:  strings.TrimSpace(payload.PrincipalID),
		PrincipalURN: strings.TrimSpace(payload.PrincipalURN),
		UserName:     officeAceUserNameFromURN(payload.PrincipalURN),
	}
}

func (d *officeAcePermissionDocument) errorText() string {
	payload := d.payload()
	if payload == nil {
		return ""
	}
	return firstNonEmpty(payload.ErrorMsg, payload.ErrorMessage, payload.ErrorReason)
}

func (d *officeAcePermissionDocument) errorCode() string {
	payload := d.payload()
	if payload == nil {
		return ""
	}
	return strings.TrimSpace(payload.ErrorCode)
}

// officeAceUserNameFromURN pulls the account name out of the principal URN the
// gateway reports.  Anything else yields an empty name, which the caller
// treats as "no display name".
func officeAceUserNameFromURN(urn string) string {
	urn = strings.TrimSpace(urn)
	lower := strings.ToLower(urn)
	if !strings.HasPrefix(lower, "iam::") && !strings.HasPrefix(lower, "sts::") {
		return ""
	}
	if i := strings.LastIndex(urn, ":user:"); i >= 0 {
		return strings.TrimSpace(urn[i+len(":user:"):])
	}
	for _, marker := range []string{"assumed-agency:", "external-user:"} {
		if i := strings.LastIndex(urn, marker); i >= 0 {
			rest := urn[i+len(marker):]
			if j := strings.Index(rest, "/"); j >= 0 {
				return strings.TrimSpace(rest[j+1:])
			}
			return ""
		}
	}
	return ""
}

// checkPermission validates the fresh credential and returns the identity plus
// (when the account is already subscribed) the model-gateway block.
func (o *officeAceSource) checkPermission(ctx context.Context, eps officeAceEndpoints, token officeAceToken) (officeAceIdentity, officeAceModelInfo, error) {
	endpoint := strings.TrimRight(eps.ClawBase, "/") + "/v1/claw/client-permission-validate"
	headers := map[string]string{"Accept": "application/json", "x-subscription-type": "v2"}
	if token.SecurityToken != "" {
		headers["X-Security-Token"] = token.SecurityToken
	}
	if token.ProjectID != "" {
		headers["X-Project-ID"] = token.ProjectID
	}
	raw, status, err := o.signedCall(ctx, http.MethodGet, endpoint, headers, "", token)
	if err != nil {
		return officeAceIdentity{}, officeAceModelInfo{}, err
	}
	var doc officeAcePermissionDocument
	if len(raw) > 0 {
		if uerr := json.Unmarshal(raw, &doc); uerr != nil && status == http.StatusOK {
			return officeAceIdentity{}, officeAceModelInfo{}, fmt.Errorf("officeace: 解析权限校验结果失败：%w", uerr)
		}
	}
	if status != http.StatusOK {
		msg := firstNonEmpty(doc.errorText(), strings.TrimSpace(string(raw)))
		if doc.errorCode() == officeAceBannedErrorCode {
			return officeAceIdentity{}, officeAceModelInfo{}, fmt.Errorf("officeace: 这个华为账号已被平台封禁：%s", truncate(msg, 200))
		}
		return officeAceIdentity{}, officeAceModelInfo{}, fmt.Errorf("officeace: 权限校验失败：%s", officeAceHTTPError(status, []byte(msg)))
	}
	return doc.identity(), doc.model(), nil
}

// subscribe opens the service for a login that has no subscription yet.  The
// desktop client shows an invitation-code page here; for an account that is
// already eligible the call without a code is enough, and anything else is
// reported to the operator verbatim.
func (o *officeAceSource) subscribe(ctx context.Context, eps officeAceEndpoints, token officeAceToken, promotionCode string) (officeAceModelInfo, error) {
	endpoint := strings.TrimRight(eps.ClawBase, "/") + "/v1/claw/client-subscription"
	headers := map[string]string{
		"Accept":              "application/json",
		"Content-Type":        "application/json;charset=utf8",
		"x-subscription-type": "v2",
	}
	if token.SecurityToken != "" {
		headers["X-Security-Token"] = token.SecurityToken
	}
	if token.ProjectID != "" {
		headers["X-Project-ID"] = token.ProjectID
	}
	body := "{}"
	if code := strings.TrimSpace(promotionCode); code != "" {
		encoded, err := json.Marshal(map[string]string{"promotion_code": code})
		if err != nil {
			return officeAceModelInfo{}, err
		}
		body = string(encoded)
	}
	raw, status, err := o.signedCall(ctx, http.MethodPost, endpoint, headers, body, token)
	if err != nil {
		return officeAceModelInfo{}, err
	}
	var doc officeAcePermissionDocument
	if len(raw) > 0 {
		if uerr := json.Unmarshal(raw, &doc); uerr != nil && status == http.StatusOK {
			return officeAceModelInfo{}, fmt.Errorf("officeace: 解析开通结果失败：%w", uerr)
		}
	}
	if status != http.StatusOK {
		msg := firstNonEmpty(doc.errorText(), strings.TrimSpace(string(raw)))
		switch doc.errorCode() {
		case "AgentArts.11000008", "AgentArts.11000009", "common.01010003", "common.01010004", "AgentArts.00000104":
			return officeAceModelInfo{}, fmt.Errorf("officeace: 这个华为账号还没有开通 OfficeAce 服务，请先在 OfficeAce 客户端里完成开通（%s）", truncate(msg, 160))
		}
		return officeAceModelInfo{}, fmt.Errorf("officeace: 开通 OfficeAce 服务失败：%s", officeAceHTTPError(status, []byte(msg)))
	}
	info := doc.payload().model()
	if !info.usable() {
		return officeAceModelInfo{}, errors.New("officeace: 开通成功，但云端没有返回模型网关凭据")
	}
	return info, nil
}

// signedCall performs one AK/SK-signed request and returns its body and status.
// A non-2xx status is data, not an error: the callers above explain it.
func (o *officeAceSource) signedCall(ctx context.Context, method, endpoint string, headers map[string]string, body string, token officeAceToken) ([]byte, int, error) {
	signed, err := signOfficeAceRequest(method, endpoint, headers, body, token.AccessKey, token.SecretKey, time.Now())
	if err != nil {
		return nil, 0, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	if reader != nil {
		req, err = http.NewRequestWithContext(reqCtx, method, endpoint, reader)
	} else {
		req, err = http.NewRequestWithContext(reqCtx, method, endpoint, nil)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("officeace: build %s request: %w", method, err)
	}
	for key, value := range signed {
		if strings.EqualFold(key, "host") {
			// Go takes the Host header from the request, not from Header.
			req.Host = value
			continue
		}
		req.Header.Set(key, value)
	}
	resp, err := o.httpClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("officeace: 调用 %s 失败：%w", endpoint, err)
	}
	defer resp.Body.Close()
	return readLimited(resp.Body, maxErrorBytes), resp.StatusCode, nil
}

// completeLogin turns a code into a stored account: exchange, validate, make
// sure a model gateway exists, then keep the application key pair.
func (o *officeAceSource) completeLogin(ctx context.Context, session *officeAceLoginSession, code string) error {
	eps := o.endpoints()
	workCtx, cancel := context.WithTimeout(ctx, officeAceExchangeTimeout)
	defer cancel()
	token, err := o.exchangeAuthorizationCode(workCtx, eps, session, code)
	if err != nil {
		return err
	}
	identity, info, err := o.checkPermission(workCtx, eps, token)
	if err != nil {
		return err
	}
	if !info.usable() {
		info, err = o.subscribe(workCtx, eps, token, "")
		if err != nil {
			return err
		}
	}
	baseURL := normalizeOfficeAceBaseURL(info.APIURLBase)
	if baseURL == "" {
		baseURL = o.source.currentBaseURL()
	}
	seed := firstNonEmpty(identity.key(), token.Subject, token.UserName, identity.UserName)
	rec, err := o.source.addAccount(workCtx, core.AccountSpec{
		ID:    officeAceAccountID(seed),
		Label: firstNonEmpty(identity.UserName, token.UserName, identity.AccountID),
		Fields: map[string]string{
			"api_key":    info.AuthInfo.AppKey,
			"app_secret": info.AuthInfo.AppSecret,
		},
	}, baseURL)
	if err != nil {
		return fmt.Errorf("officeace: 保存账号失败：%w", err)
	}
	o.persistLoginSession(workCtx, eps, rec.ID, token, session.key, identity, baseURL)
	// The desktop client creates the free personal trial after the model
	// credentials exist, so this must not be conditional on subscribe() above.
	// It is best-effort: the account can already chat even if the order call
	// fails, and a later login retries the same idempotent check.
	if err := o.ensureOfficeAceTrial(workCtx, rec.ID); err != nil {
		o.source.deps.Log("%s: signed in %s, but the free trial entitlement could not be created: %v", officeAceSourceID, rec.ID, core.Redact(err.Error()))
	}
	o.scanModels(workCtx, rec.ID)
	o.source.deps.Log("%s: Huawei account signed in (%s)", officeAceSourceID, rec.ID)
	session.finish(core.LoginSuccess, "已用华为账号登录并写入平台账号池", rec.ID)
	return nil
}

// persistLoginSession stores the cloud credential that lets the premium routes
// (每日签到/余额/套餐) be signed later.  It is best-effort: a credential the
// cloud refuses to re-mint must not turn a successful model login into a
// failure, because the account can still chat.
func (o *officeAceSource) persistLoginSession(ctx context.Context, eps officeAceEndpoints, accountID string, token officeAceToken, dpop *ecdsa.PrivateKey, identity officeAceIdentity, baseURL string) {
	if strings.TrimSpace(token.RefreshToken) != "" && strings.TrimSpace(token.RefreshExpiresAt) == "" {
		token.RefreshExpiresAt = o.source.now().UTC().Add(officeAceRefreshTTL).Format(time.RFC3339)
	}
	sess := officeAceSessionFromLogin(token, dpop, identity, officeAceRegionFromBaseURL(baseURL))
	if v3, ok := officeAceFetchV3(ctx, o.httpClient(), eps.KeystoneBase, sess.AccessKey, sess.SecretKey, sess.SecurityToken, sess.ProjectID, o.source.now()); ok {
		sess.V3AccessKey = v3.Access
		sess.V3SecretKey = v3.Secret
		sess.V3SecurityToken = v3.SecurityToken
		sess.V3ProjectID = v3.ProjectID
		sess.V3ExpiresAt = v3.ExpiresAt
	}
	if err := o.saveOfficeAceSession(accountID, sess); err != nil {
		o.source.deps.Log("%s: signed in, but the cloud session could not be stored: %v", officeAceSourceID, core.Redact(err.Error()))
	}
}

// scanModels fills the catalogue for a freshly added account.  It is
// best-effort: a stored credential must not be reported as a failed login just
// because the model list could not be read this second -- the panel's 扫描模型
// button performs the same call by hand.
func (o *officeAceSource) scanModels(ctx context.Context, id string) {
	src := o.source
	src.mu.Lock()
	var row storedProvider
	found := false
	for _, candidate := range src.rows {
		if candidate.ID == id {
			row = candidate
			found = true
			break
		}
	}
	transport := src.transportLocked()
	prov := ProviderConfig{}
	if found {
		prov = src.providerLocked(row)
	}
	generation := src.generation
	src.mu.Unlock()
	if !found {
		return
	}
	scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := src.scan(scanCtx, row, transport, prov, generation); err != nil {
		src.deps.Log("%s: signed in, but the model list will be scanned later: %v", officeAceSourceID, core.Redact(err.Error()))
	}
}

// --- crypto helpers ---------------------------------------------------------

// officeAcePrivateKeyFromScalar rebuilds the DPoP P-256 key from its 32-byte
// private scalar, which is what the stored profile keeps.
func officeAcePrivateKeyFromScalar(scalar []byte) (*ecdsa.PrivateKey, error) {
	curve := elliptic.P256()
	d := new(big.Int).SetBytes(scalar)
	if d.Sign() <= 0 || d.Cmp(curve.Params().N) >= 0 {
		return nil, errors.New("officeace: DPoP scalar is out of range")
	}
	priv := &ecdsa.PrivateKey{D: d}
	priv.PublicKey.Curve = curve
	priv.PublicKey.X, priv.PublicKey.Y = curve.ScalarBaseMult(scalar)
	return priv, nil
}

// officeAceRandomToken returns n random bytes as unpadded base64url, which is
// what the PKCE verifier needs.
func officeAceRandomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func officeAcePKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// officeAceAuthorizeURL builds the Huawei login page URL.  The oauth2
// authorize URL travels inside the login page's "service" parameter, exactly
// as the desktop client composes it.
//
// The login page is itself wrapped in Huawei's logout endpoint.  A browser
// that still holds the previous Huawei session answers the authorize request
// from that session and signs the operator straight back into the account they
// were trying to leave, so the link has to clear the session before it asks
// for one.  /authui/logout accepts a "service" return URL, so a single click
// logs out and then lands on the login page with the original request intact.
func officeAceAuthorizeURL(eps officeAceEndpoints, state, challenge string) string {
	authBase := strings.TrimRight(eps.AuthBase, "/")
	params := url.Values{
		"client_id":             {eps.ClientID},
		"code_challenge":        {challenge},
		"code_challenge_method": {"SHA-256"},
		"state":                 {state},
		"scope":                 {"openid"},
		"redirect_uri":          {eps.RedirectURI},
		"response_type":         {"code"},
	}
	inner := authBase + "/authui/v1/oauth2/authorize?" + params.Encode()
	background := "https://res.hc-cdn.com/AgentArts-Console/26.4.7/hws/assets/login/bg2.png"
	portal := "https://res.hc-cdn.com/AgentArts-Console/26.4.7/hws/assets/login/ad2.png"
	login := authBase + "/authui/login.html?service=" + url.QueryEscape(inner) +
		"&background_img_url=" + url.QueryEscape(background) +
		"&portal_img_url=" + url.QueryEscape(portal)
	return authBase + "/authui/logout?service=" + url.QueryEscape(login)
}

// officeAceDPoPProof signs one DPoP proof JWT (ES256, raw r||s, exactly as
// WebCrypto produces it) for the given method and URL.
func officeAceDPoPProof(key *ecdsa.PrivateKey, htm, htu string, at time.Time) (string, error) {
	if key == nil {
		return "", errors.New("officeace: the sign-in key is missing")
	}
	jti, err := officeAceRandomToken(16)
	if err != nil {
		return "", fmt.Errorf("officeace: no entropy for the proof: %w", err)
	}
	x := base64.RawURLEncoding.EncodeToString(key.PublicKey.X.FillBytes(make([]byte, 32)))
	y := base64.RawURLEncoding.EncodeToString(key.PublicKey.Y.FillBytes(make([]byte, 32)))
	header := map[string]any{
		"typ": "dpop+jwt",
		"alg": "ES256",
		"jwk": map[string]string{"kty": "EC", "crv": "P-256", "x": x, "y": y},
	}
	payload := map[string]any{
		"htm": htm,
		"htu": htu,
		"iat": at.Unix(),
		"jti": jti,
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("officeace: signing the DPoP proof: %w", err)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// officeAceHTTPError renders the error line of a non-2xx answer without
// dumping a whole page into the panel.
func officeAceHTTPError(status int, raw []byte) string {
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d %s", status, truncate(core.Redact(msg), 240))
}
