package lobsterai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// maxErrorBody bounds what a failing upstream can make us buffer.  A vendor
// error page is a few hundred bytes; a megabyte of it is an attack, not a
// message.
const maxErrorBody = 1 << 20

// envelope is the {code, msg, data} wrapper every LobsterAI endpoint uses --
// every endpoint except the chat proxy, which answers with a bare SSE stream.
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// upstreamError is a decoded failure: either an envelope with a non-zero code
// or a non-2xx status whose body we could read.
type upstreamError struct {
	Op     string
	Code   int
	Msg    string
	Status int
}

func (e *upstreamError) Error() string {
	var b strings.Builder
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, "HTTP %d ", e.Status)
	}
	if e.Code != 0 {
		fmt.Fprintf(&b, "code %d ", e.Code)
	}
	msg := strings.TrimSpace(e.Msg)
	if msg == "" {
		msg = "upstream refused the request"
	}
	b.WriteString(msg)
	return b.String()
}

// decodeEnvelope parses the wrapper and returns the raw data payload.
//
// code != 0 is a failure, and so is a missing data payload on an endpoint that
// must return one: the vendor signals a dead access token both ways.
// UseNumber keeps a large configRevision from being rounded through float64.
func decodeEnvelope(op string, body []byte) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, &upstreamError{Op: op, Msg: "empty response body"}
	}
	var env envelope
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil {
		return nil, &upstreamError{Op: op, Msg: "malformed response: " + truncate(string(trimmed), 200)}
	}
	if env.Code != 0 {
		return nil, &upstreamError{Op: op, Code: env.Code, Msg: env.Msg}
	}
	if len(bytes.TrimSpace(env.Data)) == 0 || string(bytes.TrimSpace(env.Data)) == "null" {
		return nil, &upstreamError{Op: op, Code: env.Code, Msg: "response carried no data (the access token may be dead)"}
	}
	return env.Data, nil
}

// decodeEnvelopeObject is decodeEnvelope for the endpoints whose data is an
// object.
func decodeEnvelopeObject(op string, body []byte) (map[string]any, error) {
	data, err := decodeEnvelope(op, body)
	if err != nil {
		return nil, err
	}
	obj := objectOf(anyOf(data))
	if obj == nil {
		return nil, &upstreamError{Op: op, Msg: "response data is not an object"}
	}
	return obj, nil
}

// anyOf decodes raw JSON into the generic Go shape (with json.Number, so
// numbers survive a round trip).
func anyOf(raw json.RawMessage) any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	return v
}

// requestSpec describes one upstream call.
type requestSpec struct {
	method  string
	url     string
	body    []byte
	bearer  string
	version string
	accept  string
	timeout time.Duration
}

// newRequest builds an upstream request with the headers the vendor expects.
//
// The two X-LobsterAI headers are sent on every authenticated call: the
// capability token decides which models the backend will serve and the version
// is validated by the activity endpoints.
func (c *Client) newRequest(ctx context.Context, spec requestSpec) (*http.Request, error) {
	var reader io.Reader
	if spec.body != nil {
		reader = bytes.NewReader(spec.body)
	}
	req, err := http.NewRequestWithContext(ctx, spec.method, spec.url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", firstNonEmpty(spec.accept, "application/json"))
	if spec.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	if spec.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+spec.bearer)
		req.Header.Set("X-LobsterAI-Client-Capabilities", c.cfg.Capabilities)
		req.Header.Set("X-LobsterAI-Client-Version", firstNonEmpty(spec.version, c.clientVersion(ctx)))
	}
	for k, v := range c.cfg.ExtraHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

// do performs one upstream call and returns the raw response.  The caller owns
// the body.
//
// The per-call timeout must outlive this function.  The body is read by the
// CALLER -- minutes later, for the SSE chat stream -- so a `defer cancel()`
// here would abort every read with `context canceled` the moment `do`
// returned.  Instead the cancel is tied to the body: it fires when the caller
// closes it, which every call site already does.  A caller that forgets still
// cannot leak, because the timeout itself eventually fires.
func (c *Client) do(ctx context.Context, spec requestSpec) (*http.Response, error) {
	req, err := c.newRequest(ctx, spec)
	if err != nil {
		return nil, err
	}
	var cancel context.CancelFunc
	if spec.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, spec.timeout)
		req = req.WithContext(ctx)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	if cancel != nil {
		resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, nil
}

// cancelBody releases a request context's cancel func when the caller closes
// the response body, so the per-call timeout stays armed for exactly as long as
// somebody can still read the response.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cancel)
	return err
}

// doJSON performs one upstream call and returns the status and body.
func (c *Client) doJSON(ctx context.Context, spec requestSpec) (int, []byte, error) {
	resp, err := c.do(ctx, spec)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, readLimited(resp.Body, maxErrorBody), nil
}

// --- token plumbing ---------------------------------------------------------

// tokenPayload is the data object shared by exchange and refresh.
type tokenPayload struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    any    `json:"expiresIn"`
	User         struct {
		ID       any    `json:"id"`
		YID      any    `json:"yid"`
		UserID   any    `json:"userId"`
		Nickname string `json:"nickname"`
	} `json:"user"`
}

// exchangeRequest is the login exchange body.  All five fields are mandatory;
// uuid is not omitempty here for that reason.
type exchangeRequest struct {
	AuthCode      string `json:"authCode"`
	FirstKeyfrom  string `json:"firstKeyfrom"`
	LatestKeyfrom string `json:"latestKeyfrom"`
	UUID          string `json:"uuid"`
	Version       string `json:"version"`
}

// refreshRequest is the renewal body: the keyfrom identity replayed from the
// credential, plus the refresh token.  uuid and userId are only sent when the
// credential has them.
//
// There is deliberately no Authorization header on this call -- see
// (*Client).refreshCredential.
type refreshRequest struct {
	FirstKeyfrom  string `json:"firstKeyfrom"`
	LatestKeyfrom string `json:"latestKeyfrom"`
	Version       string `json:"version"`
	UUID          string `json:"uuid,omitempty"`
	UserID        string `json:"userId,omitempty"`
	RefreshToken  string `json:"refreshToken"`
}

// exchange redeems an authCode for a credential.
//
// firstKeyfrom is minted once, at login, and latestKeyfrom is minted now; both
// are persisted because every later renewal must replay them verbatim.
func (c *Client) exchange(ctx context.Context, authCode, uuid, firstKeyfrom, version string) (accountRecord, error) {
	now := c.now()
	if strings.TrimSpace(authCode) == "" {
		return accountRecord{}, fmt.Errorf("%w: the login callback carried no authCode", core.ErrNotConfigured)
	}
	if uuid == "" {
		uuid = newUUID()
	}
	if firstKeyfrom == "" {
		firstKeyfrom = millisString(now)
	}
	if !validVersion(version) {
		version = c.cfg.ClientVersion
	}
	// At sign-in both keyfrom values are the same instant.  Minting a second
	// value from a fresh clock read would drift by a millisecond and store a
	// credential whose first and latest keyfrom disagree, which the renewal
	// path then replays forever.
	body, err := json.Marshal(exchangeRequest{
		AuthCode:      authCode,
		FirstKeyfrom:  firstKeyfrom,
		LatestKeyfrom: firstKeyfrom,
		UUID:          uuid,
		Version:       version,
	})
	if err != nil {
		return accountRecord{}, err
	}
	status, raw, err := c.doJSON(ctx, requestSpec{
		method:  http.MethodPost,
		url:     c.cfg.exchangeURL(),
		body:    body,
		timeout: c.cfg.modelsTimeout(),
	})
	if err != nil {
		return accountRecord{}, fmt.Errorf("exchange: %w", err)
	}
	if status != http.StatusOK {
		return accountRecord{}, c.classifyHTTP("exchange", "", status, raw)
	}
	data, err := decodeEnvelopeObject("exchange", raw)
	if err != nil {
		return accountRecord{}, c.classifyUpstream("", err)
	}
	var payload tokenPayload
	if err := remarshal(data, &payload); err != nil {
		return accountRecord{}, fmt.Errorf("exchange: %w", err)
	}
	if payload.AccessToken == "" {
		return accountRecord{}, &upstreamError{Op: "exchange", Msg: "no accessToken in response"}
	}
	// The uid fallback chain has exactly four levels, in this order.  Adding a
	// fifth (a JWT subject, say) would give one account two identities and
	// silently split its credits.
	uid := firstNonEmpty(
		asString(payload.User.ID),
		asString(payload.User.UserID),
		asString(payload.User.YID),
		tokenFingerprint(payload.AccessToken),
	)
	label := firstNonEmpty(payload.User.Nickname, uid)
	rec := accountRecord{
		ID:            accountID(uid),
		Label:         label,
		UID:           uid,
		UserID:        asString(payload.User.UserID),
		UUID:          uuid,
		AccessToken:   payload.AccessToken,
		RefreshToken:  payload.RefreshToken,
		ExpiresAt:     expiryOf(payload, now),
		FirstKeyfrom:  firstKeyfrom,
		LatestKeyfrom: millisString(now),
		Nickname:      payload.User.Nickname,
		Enabled:       true,
		AddedAt:       now.UTC().Format(time.RFC3339),
	}
	return rec, nil
}

// refreshCredential renews one account's access token.
//
// The call carries NO Authorization header: the endpoint authenticates on the
// refresh token alone, and the vendor's own client is explicit that a dead
// access token must not block its own renewal.
func (c *Client) refreshCredential(ctx context.Context, acct *accountRecord) error {
	if acct == nil {
		return fmt.Errorf("%w: no account to renew", core.ErrNotConfigured)
	}
	if acct.RefreshToken == "" {
		return &upstreamError{Op: "refresh", Msg: "credential has no refresh token; log in again"}
	}
	body, err := json.Marshal(refreshRequest{
		FirstKeyfrom:  acct.FirstKeyfrom,
		LatestKeyfrom: acct.LatestKeyfrom,
		Version:       c.clientVersion(ctx),
		UUID:          acct.UUID,
		UserID:        acct.UserID,
		RefreshToken:  acct.RefreshToken,
	})
	if err != nil {
		return err
	}
	status, raw, err := c.doJSON(ctx, requestSpec{
		method:  http.MethodPost,
		url:     c.cfg.refreshURL(),
		body:    body,
		timeout: c.cfg.modelsTimeout(),
	})
	if err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	if status != http.StatusOK {
		return c.classifyHTTP("refresh", acct.ID, status, raw)
	}
	data, err := decodeEnvelopeObject("refresh", raw)
	if err != nil {
		return c.classifyUpstream(acct.ID, err)
	}
	var payload tokenPayload
	if err := remarshal(data, &payload); err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	if payload.AccessToken == "" {
		return &upstreamError{Op: "refresh", Msg: "no accessToken in response — log in again"}
	}
	now := c.now()
	expiresAt := expiryOf(payload, now)
	if !c.pool.updateTokens(acct.ID, payload.AccessToken, payload.RefreshToken, expiresAt) {
		return fmt.Errorf("refresh: account %q vanished while renewing", acct.ID)
	}
	c.persist()
	c.deps.Log("lobsterai: renewed %s (expires %s)", acct.ID, firstNonEmpty(expiresAt, "unknown"))
	return nil
}

// ensureFresh renews an account whose token has expired or is about to.
//
// A credential with no expiry at all is treated as expired: the field is
// optional in the config, and a token with unknown lifetime is exactly the case
// where a renewal is cheapest to attempt.
func (c *Client) ensureFresh(ctx context.Context, acct *accountRecord) error {
	if acct == nil {
		return fmt.Errorf("%w: no account", core.ErrNotConfigured)
	}
	if acct.RefreshToken == "" {
		return nil
	}
	if !c.needsRenewal(acct) {
		return nil
	}
	return c.refreshCredential(ctx, acct)
}

// needsRenewal reports whether an account's access token is inside the renewal
// margin.
func (c *Client) needsRenewal(acct *accountRecord) bool {
	exp, ok := parseExpiry(acct.ExpiresAt)
	if !ok {
		return true
	}
	return !c.now().Add(c.cfg.refreshMargin()).Before(exp)
}

// expiryOf prefers expiresIn and falls back to the JWT exp claim, which is what
// the vendor's own client does.
func expiryOf(payload tokenPayload, now time.Time) string {
	if secs, ok := toInt(payload.ExpiresIn); ok && secs > 0 {
		return now.Add(time.Duration(secs) * time.Second).UTC().Format(time.RFC3339)
	}
	if exp, ok := jwtExpiry(payload.AccessToken); ok {
		return exp.UTC().Format(time.RFC3339)
	}
	return ""
}

// parseExpiry reads a stored expiry.  Both RFC3339 and a bare unix second count
// are accepted, because a hand-written config is likely to carry the latter.
func parseExpiry(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if secs > 1e12 { // milliseconds
			return time.UnixMilli(secs), true
		}
		return time.Unix(secs, 0), true
	}
	return time.Time{}, false
}

// jwtExpiry decodes the exp claim of an unverified JWT.  The signature is not
// checked on purpose: this is the vendor's token, we only need to know when it
// stops working, and the value is never trusted for anything but scheduling.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(parts[1]); err != nil {
			return time.Time{}, false
		}
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		return time.Time{}, false
	}
	secs, err := claims.Exp.Int64()
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// remarshal re-encodes a decoded object into a typed struct.
func remarshal(v any, out any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// millisString renders a timestamp the way the vendor's client does: unix
// milliseconds as a string.
func millisString(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

// --- client version ---------------------------------------------------------

// versionResponse is the update manifest.  code and msg sit OUTSIDE data on
// this endpoint, which is why it is decoded by hand rather than through
// decodeEnvelope.
type versionResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Value struct {
			Version string `json:"version"`
		} `json:"value"`
	} `json:"data"`
}

// fetchVersion asks the update service for the current client version.
//
// The result is only used when it matches the vendor's own version grammar: a
// captive portal that answers 200 with an HTML page must not become the version
// we send.
func (c *Client) fetchVersion(ctx context.Context) (string, bool) {
	status, raw, err := c.doJSON(ctx, requestSpec{
		method:  http.MethodGet,
		url:     c.cfg.VersionURL,
		timeout: c.cfg.versionTimeout(),
	})
	if err != nil {
		return "", false
	}
	if status != http.StatusOK {
		return "", false
	}
	var parsed versionResponse
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return "", false
	}
	// code and msg sit OUTSIDE data on this endpoint, but it is still an
	// envelope: a non-zero code means the payload is not to be trusted.
	if parsed.Code != 0 {
		return "", false
	}
	v := strings.TrimSpace(parsed.Data.Value.Version)
	if !validVersion(v) {
		return "", false
	}
	return v, true
}

// clientVersion returns the version to send, from cache only.
//
// It never fetches: this is called while building the headers of a chat
// request, and a chat must not wait on a third-party manifest service.  The
// configured fallback is what answers until a background refresh lands.
func (c *Client) clientVersion(ctx context.Context) string {
	c.versionMu.Lock()
	v := c.version
	c.versionMu.Unlock()
	if v != "" {
		return v
	}
	return c.cfg.ClientVersion
}

// versionState reports the cached version and whether it is stale.
func (c *Client) versionState() (string, bool) {
	c.versionMu.Lock()
	defer c.versionMu.Unlock()
	stale := c.version == "" || c.now().Sub(c.versionAt) >= c.cfg.versionTTL()
	return c.version, stale
}

// storeVersion caches a fetched version.
func (c *Client) storeVersion(v string) {
	if !validVersion(v) {
		return
	}
	c.versionMu.Lock()
	c.version = v
	c.versionAt = c.now()
	c.versionMu.Unlock()
}

// refreshVersionIfStale fetches the version when the cache is cold or old.
// Callers treat a failure as "keep the fallback".
func (c *Client) refreshVersionIfStale(ctx context.Context) {
	if _, stale := c.versionState(); !stale {
		return
	}
	if v, ok := c.fetchVersion(ctx); ok {
		c.storeVersion(v)
	}
}

// refreshVersionAsync is the chat-path variant: it must not block, so the fetch
// happens on a guarded goroutine and the current (possibly fallback) value is
// used for this request.
//
// One fetch at a time, and the flag is cleared when it lands, so a long-lived
// process keeps its cached version fresh instead of freezing the first answer.
func (c *Client) refreshVersionAsync() {
	if _, stale := c.versionState(); !stale {
		return
	}
	c.versionMu.Lock()
	if c.versionFetching {
		c.versionMu.Unlock()
		return
	}
	c.versionFetching = true
	c.versionMu.Unlock()

	core.GoSafe("lobsterai version refresh", func(msg string) { c.deps.Log("%s", msg) }, func() {
		defer func() {
			c.versionMu.Lock()
			c.versionFetching = false
			c.versionMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.versionTimeout())
		defer cancel()
		c.refreshVersionIfStale(ctx)
	})
}
