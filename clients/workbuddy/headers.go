package workbuddy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Header construction.  Ported from the reference implementation
// (internal/upstream/headers.go, MIT).  The reference calls into its own
// `session` package for message ids and into `auth.Auth` for identity; here the
// same values come from the Auth accessors above and from newMessageID below,
// so the module stays self-contained.

const (
	// defaultClientVersion is the CN WorkBuddy Desktop version segment used in
	// the outbound UA and in the X-IDE-Version attribution header.  Matches the
	// official CN desktop distribution (5.7.7).
	defaultClientVersion = "5.7.7"
	// defaultCliVersion is the CN `CLI/<ver>` UA segment.  Matches the official
	// bundled CN CLI (2.137.1).
	defaultCliVersion = "2.137.1"

	// defaultClientVersionGlobal is the international ("WorkBuddy AI") desktop
	// version.  The 5.8.4 bundle is what the official global client ships today,
	// and a global request that still announces the retired 5.7.7 pair is
	// answered with business code 11-128 ("request illegal").
	defaultClientVersionGlobal = "5.8.4"
	// defaultCliVersionGlobal is the international `CLI/<ver>` UA segment
	// (bundled codebuddy CLI 2.161.0).
	defaultCliVersionGlobal = "2.161.0"
	// globalUAPlatform matches the official WorkBuddy VLM UA builder, which
	// repeats the product name on both realms. The bundled CLI version still
	// distinguishes the international 5.8.4 pair from the domestic 5.7.7 pair.
	globalUAPlatform = "WorkBuddy"

	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"

	// attributionDefaultName spoofs the official desktop fingerprint so upstream
	// usage attribution does not look like a gateway.
	attributionDefaultName = "WorkBuddy"
)

// originRefererFor returns the Origin/Referer base for an account's realm.
func originRefererFor(a *Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}
	return originRefererCN
}

// clientVersionFor returns the desktop version for an account's realm.  An
// explicit client_version in the config wins for both realms; otherwise each
// realm gets the version its official client ships.
func (u *Upstream) clientVersionFor(a *Auth) string {
	if u != nil && u.ClientVersion != "" {
		return u.ClientVersion
	}
	if a != nil && a.IsGlobal() {
		return defaultClientVersionGlobal
	}
	return defaultClientVersion
}

// clientVersion is the CN desktop version.  It also feeds the CN web-login URL,
// which never serves the international realm.
func (u *Upstream) clientVersion() string { return u.clientVersionFor(nil) }

// cliVersionFor returns the bundled CLI version for an account's realm, with the
// same explicit-config-wins rule as clientVersionFor.
func (u *Upstream) cliVersionFor(a *Auth) string {
	if u != nil && u.CliVersion != "" {
		return u.CliVersion
	}
	if a != nil && a.IsGlobal() {
		return defaultCliVersionGlobal
	}
	return defaultCliVersion
}

// cliVersion is the CN bundled CLI version.
func (u *Upstream) cliVersion() string { return u.cliVersionFor(nil) }

// defaultWorkBuddyUAFor builds the official desktop UA. The desktop/CLI
// version pair differs per realm: CN sends `WorkBuddy/5.7.7
// WorkBuddy/5.7.7 CLI/2.137.1`, while the official international VLM sends
// `WorkBuddy/5.8.4 WorkBuddy/5.8.4 CLI/2.161.0`. Announcing the retired
// pair on a global request is answered with business code 11-128 ("request
// illegal").
func (u *Upstream) defaultWorkBuddyUAFor(a *Auth) string {
	platform := "WorkBuddy"
	if a != nil && a.IsGlobal() {
		platform = globalUAPlatform
	}
	version := u.clientVersionFor(a)
	return "WorkBuddy/" + version + " " + platform + "/" + version + " CLI/" + u.cliVersionFor(a)
}

// userAgent is the outbound UA: explicit configuration wins, else the
// per-realm desktop default.
func (u *Upstream) userAgent(a *Auth) string {
	if u != nil && u.UserAgent != "" {
		return u.UserAgent
	}
	return u.defaultWorkBuddyUAFor(a)
}

// billingUA is the single-segment UA used for the whitelist endpoints
// (billing/checkin).  It is suppressed when client_name is explicitly "SaaS".
// The version still follows the realm: an international account must not send
// the retired CN version on a billing call.
func (u *Upstream) billingUA(a *Auth) string {
	if u == nil || u.attributionClientName() == "SaaS" {
		return ""
	}
	return "WorkBuddy/" + u.clientVersionFor(a)
}

func (u *Upstream) attributionClientName() string {
	if u != nil && u.ClientName != "" {
		return u.ClientName
	}
	return attributionDefaultName
}

// resolveDeviceToken picks the X-Device-Token value: per-account > global
// config > file.  Empty means "do not inject the header".
func (u *Upstream) resolveDeviceToken(a *Auth) string {
	if a != nil {
		if tok := a.DeviceTokenValue(); tok != "" {
			return tok
		}
	}
	if u != nil && u.DeviceToken != "" {
		return u.DeviceToken
	}
	if u != nil && u.DeviceTokenFile != "" {
		return u.readDeviceTokenFile(u.DeviceTokenFile)
	}
	return ""
}

func (u *Upstream) injectDeviceToken(req *http.Request, a *Auth) {
	if tok := u.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

const (
	// deviceTokenFileTTL bounds how stale a cached device token may be.  Without
	// it the file is opened and read on every single chat request, and the file
	// is not expected to change while the process runs.
	deviceTokenFileTTL = 5 * time.Minute
	// deviceTokenFileMaxLen is the largest file we are willing to treat as a
	// token.  Anything bigger is a mistake — a log, a keychain dump, a
	// directory — and sending its first bytes as X-Device-Token would be worse
	// than sending no header at all.
	deviceTokenFileMaxLen = 1024
)

// errDeviceTokenTooLarge marks a file too big to be a token.  The header is
// dropped; the file must never be truncated into one.
var errDeviceTokenTooLarge = errors.New("device token file is too large")

// deviceTokenFileEntry is one cached read.  Failures cache the empty string
// too, so a broken path costs one stat per TTL instead of one per request.
type deviceTokenFileEntry struct {
	value  string
	readAt time.Time
}

// deviceTokenFileCache memoises readDeviceTokenFile by path.
var deviceTokenFileCache = struct {
	mu sync.Mutex
	m  map[string]deviceTokenFileEntry
}{m: make(map[string]deviceTokenFileEntry)}

// readDeviceTokenFile reads a one-line device token file, remembering the
// result for deviceTokenFileTTL.  A missing, unreadable or oversized file
// degrades to the empty string — that is, no header — rather than failing the
// request.
func (u *Upstream) readDeviceTokenFile(path string) string {
	if path == "" {
		return ""
	}

	now := time.Now()
	deviceTokenFileCache.mu.Lock()
	e, ok := deviceTokenFileCache.m[path]
	deviceTokenFileCache.mu.Unlock()
	if ok && now.Sub(e.readAt) < deviceTokenFileTTL {
		return e.value
	}

	tok, err := readTrimmedFile(path)
	// A file that does not exist yet is the normal "not configured" state and
	// must stay quiet; anything else means the configured path is wrong.
	if err != nil && !os.IsNotExist(err) {
		u.log("workbuddy: device token file %s ignored: %v", path, err)
	}

	deviceTokenFileCache.mu.Lock()
	deviceTokenFileCache.m[path] = deviceTokenFileEntry{value: tok, readAt: now}
	deviceTokenFileCache.mu.Unlock()
	return tok
}

// readTrimmedFile returns the trimmed contents of path.  The size guard runs
// before the read, so an accidentally huge file is never loaded at all.
func readTrimmedFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > deviceTokenFileMaxLen {
		return "", errDeviceTokenTooLarge
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// deriveAccountStableID derives a stable 36-hex device/session fingerprint from
// the account uid.  It is deliberately constant across restarts (unlike the
// process-scoped session salt) so one account always looks like one device.
func deriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

func uuidV4ForKey(key string) string {
	raw, err := hex.DecodeString(requestIDForKey(key))
	if err != nil || len(raw) != 16 {
		raw = make([]byte, 16)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:])
}

func (u *Upstream) injectAccountStableHeaders(req *http.Request, a *Auth) {
	uid := a.UIDValue()
	if uid == "" {
		return
	}
	req.Header.Set("X-Machine-ID", deriveAccountStableID(uid, "machine"))
	req.Header.Set("X-Session-ID", deriveAccountStableID(uid, "session"))
}

// acceptLanguageFor switches the language tag by realm, matching the official
// client per account domain.
func acceptLanguageFor(a *Auth) string {
	if a != nil && a.IsGlobal() {
		return "en-US"
	}
	return "zh-CN"
}

// CommonHeaders sets the headers shared by every API call.
func (u *Upstream) CommonHeaders(req *http.Request, a *Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", u.userAgent(a))
	// X-CodeBuddy-Request is the official client's risk-control gate header and
	// is required on every API request.
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	u.injectAccountStableHeaders(req, a)
}

// ChatMeta carries the per-conversation header family.  ConversationRequestID
// is the aggregation key for one user send (all tool calls / retries / account
// rotations reuse it); TraceID is an inbound passthrough that falls back to it.
type ChatMeta struct {
	ConversationID        string
	ConversationRequestID string
	ConversationKey       string
	TraceID               string
	// EffortFinal tells the body pipeline that the gateway already resolved
	// the reasoning level from the panel's authoritative vocabulary. The
	// module must not re-snap it against a possibly older cached ladder.
	EffortFinal bool
}

// ChatHeaders adds the chat-specific account headers on top of CommonHeaders.
// Absent fields use the X-No-* convention, matching the official CLI.
func (u *Upstream) ChatHeaders(req *http.Request, a *Auth, clientIP string, meta ChatMeta) {
	u.CommonHeaders(req, a)
	req.Header.Set("Accept", "application/json, text/event-stream")
	if a != nil && a.IsGlobal() {
		req.Header.Del("Origin")
		req.Header.Del("Referer")
		req.Header.Del("X-Machine-ID")
		req.Header.Del("X-Session-ID")
	}
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if uid := a.UIDValue(); uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	// Security red line: never carry X-Refresh-Token on a chat request.
	u.injectIdentityHeaders(req, a)
	if a != nil && !a.IsGlobal() {
		if eid := a.EnterpriseIDValue(); eid != "" {
			req.Header.Set("X-Enterprise-Id", eid)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		if d := a.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d)
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
	} else {
		u.injectGlobalChatHeaders(req, a)
	}
	u.injectAttribution(req, a)
	u.injectClientIP(req, clientIP)
	u.injectDeviceToken(req, a)
	u.injectConversationHeaders(req, a, meta)
}

// injectGlobalChatHeaders declares the international client shape for accounts
// with no enterprise id.
// injectIdentityHeaders mirrors the official runIdentityHeaders() helper.
// Only fields actually present on the stored account are sent; an absent
// method or idSource is not guessed.
func (u *Upstream) injectIdentityHeaders(req *http.Request, a *Auth) {
	if a == nil {
		return
	}
	uid, enterpriseID := a.UIDValue(), a.EnterpriseIDValue()
	method, idSource := a.AuthMethodValue(), a.IdSourceValue()
	if method != "" {
		req.Header.Set("X-Auth-Method", method)
	}
	if idSource != "" {
		req.Header.Set("X-Id-Source", idSource)
	}
	if uid == "" || (enterpriseID == "" && idSource == "" && method == "") {
		return
	}
	info := map[string]string{"uin": uid}
	if enterpriseID != "" {
		info["owner_uin"] = enterpriseID
	}
	if idSource != "" {
		info["id_source"] = idSource
	}
	if method != "" {
		info["token_source"] = method
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return
	}
	req.Header.Set("X-Userinfo", base64.StdEncoding.EncodeToString(raw))
}

func (u *Upstream) injectGlobalChatHeaders(req *http.Request, a *Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	req.Header.Set("X-No-Enterprise-Id", "1")
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// injectConversationHeaders injects the official client conversation family.
func (u *Upstream) injectConversationHeaders(req *http.Request, a *Auth, meta ChatMeta) {
	convReqID := strings.TrimSpace(meta.ConversationRequestID)
	conversationKey := strings.TrimSpace(meta.ConversationKey)
	conversationID := strings.TrimSpace(meta.ConversationID)
	if convReqID == "" {
		convReqID = newMessageID()
	}
	if conversationKey == "" {
		conversationKey = conversationID
	}
	if conversationKey == "" {
		conversationKey = convReqID
	}
	if conversationID == "" && a != nil && a.IsGlobal() {
		conversationID = requestIDForKey(conversationKey)
	}

	messageID := newMessageID()
	if conversationID != "" {
		req.Header.Set("X-Conversation-ID", conversationID)
	}
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Agent-Intent", "craft")

	global := a != nil && a.IsGlobal()
	if global {
		req.Header.Set("X-Private-Data", "true")
		req.Header.Set("acp-connection-id", uuidV4ForKey(conversationKey))
	} else {
		req.Header.Set("X-Agent-Type", "main")
		req.Header.Set("X-Private-Data", "false")
	}

	traceID := meta.TraceID
	if !validTraceID(traceID) {
		traceID = messageID
	}
	spanID := newMessageID()[:16]
	parentSpanID := newMessageID()[:16]
	req.Header.Set("X-Trace-ID", traceID)
	req.Header.Set("X-B3-TraceId", traceID)
	req.Header.Set("X-B3-SpanId", spanID)
	req.Header.Set("X-B3-Sampled", "1")
	if global {
		req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
		req.Header.Set("X-B3-ParentSpanId", parentSpanID)
		req.Header.Set("b3", traceID+"-"+spanID+"-1-"+parentSpanID)
	} else {
		req.Header.Set("b3", traceID+"-"+spanID+"-1")
	}
}

// validTraceID reports whether s is a legal B3 trace id (16 or 32 hex chars).
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// injectAttribution injects the usage attribution headers.  The default spoofs
// the official desktop fingerprint; client_name="SaaS" restores the plain
// gateway shape.
func (u *Upstream) injectAttribution(req *http.Request, a *Auth) {
	name := u.attributionClientName()
	if name == "WorkBuddy" {
		if a != nil && a.IsGlobal() {
			// The international client tags every model call it makes as a
			// SaaS conversation: the CLI sets X-Agent-Purpose from the session
			// purpose and its product interceptor fills X-Product from the
			// deployment type (SaaS).  Both are load-bearing for the
			// international risk control, which answers without them with the
			// non-official-client refusal (11-128).
			req.Header.Set("X-Agent-Purpose", "conversation")
			req.Header.Set("X-Product", "SaaS")
		}
		// The CN main desktop conversation adapter does not set X-Product or
		// X-Agent-Purpose. X-IDE-* is set from the client info provider on both
		// realms.
		req.Header.Set("X-IDE-Name", name)
		req.Header.Set("X-IDE-Type", name)
		req.Header.Set("X-IDE-Version", u.clientVersionFor(a))
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", u.clientVersionFor(a))
	req.Header.Set("X-Product", name)
}
func (u *Upstream) injectClientIP(req *http.Request, clientIP string) {
	if u == nil || !u.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP pulls the first hop of X-Forwarded-For, falling back to
// X-Real-IP.
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	return strings.TrimSpace(r.Header.Get("X-Real-IP"))
}

// BillingHeaders are the headers for the billing/checkin endpoints.
func (u *Upstream) BillingHeaders(req *http.Request, a *Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	if u != nil && u.UserAgent != "" {
		req.Header.Set("User-Agent", u.UserAgent)
	} else if ua := u.billingUA(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if uid := a.UIDValue(); uid != "" {
		req.Header.Set("X-User-Id", uid)
	}
	if eid := a.EnterpriseIDValue(); eid != "" {
		req.Header.Set("X-Enterprise-Id", eid)
		req.Header.Set("X-Tenant-Id", eid)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	u.injectDeviceToken(req, a)
}

// RefreshHeaders are the headers exclusive to the token refresh endpoint.
// X-Refresh-Token must only ever appear here.
func (u *Upstream) RefreshHeaders(req *http.Request, a *Auth) {
	u.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshTokenValue())
	if eid := a.EnterpriseIDValue(); eid != "" {
		req.Header.Set("X-Enterprise-Id", eid)
	}
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}

// --- message ids -----------------------------------------------------------

var msgCounter uint64

// newMessageID returns a 32-hex message id, matching the shape the official
// client uses for X-Request-ID / X-Conversation-Message-ID.  It never panics:
// if the system entropy source fails it falls back to a hash of time+counter.
func newMessageID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d-%d", time.Now().UnixNano(), atomic.AddUint64(&msgCounter, 1))))
		copy(b[:], sum[:16])
	}
	return hex.EncodeToString(b[:])
}
