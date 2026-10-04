package workbuddy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	// defaultClientVersion is the WorkBuddy Desktop version segment used in the
	// outbound UA and in the X-IDE-Version attribution header.  Matches the
	// official desktop distribution (5.5.4).
	defaultClientVersion = "5.5.4"
	// defaultCliVersion is the `CLI/<ver>` UA segment.  Matches the official
	// bundled CLI (2.137.1).
	defaultCliVersion = "2.137.1"

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

func (u *Upstream) clientVersion() string {
	if u != nil && u.ClientVersion != "" {
		return u.ClientVersion
	}
	return defaultClientVersion
}

func (u *Upstream) cliVersion() string {
	if u != nil && u.CliVersion != "" {
		return u.CliVersion
	}
	return defaultCliVersion
}

// defaultWorkBuddyUAFor builds the official desktop UA.  The platform segment
// differs per realm: `WorkBuddy` for CN, `WorkBuddy AI` for the international
// product.  Sending the wrong one can trip upstream risk control (403 code
// 11140 "request illegal").
func (u *Upstream) defaultWorkBuddyUAFor(a *Auth) string {
	platform := "WorkBuddy"
	if a != nil && a.IsGlobal() {
		platform = "WorkBuddy AI"
	}
	return "WorkBuddy/" + u.clientVersion() + " " + platform + "/" + u.clientVersion() + " CLI/" + u.cliVersion()
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
func (u *Upstream) billingUA() string {
	if u == nil || u.attributionClientName() == "SaaS" {
		return ""
	}
	return "WorkBuddy/" + u.clientVersion()
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
	TraceID               string
}

// ChatHeaders adds the chat-specific account headers on top of CommonHeaders.
// Absent fields use the X-No-* convention, matching the official CLI.
func (u *Upstream) ChatHeaders(req *http.Request, a *Auth, clientIP string, meta ChatMeta) {
	u.CommonHeaders(req, a)
	req.Header.Set("Accept", "application/json, text/event-stream")
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
	u.injectAttribution(req)
	u.injectClientIP(req, clientIP)
	u.injectDeviceToken(req, a)
	u.injectConversationHeaders(req, meta)
}

// injectGlobalChatHeaders declares the international client shape for accounts
// with no enterprise id.
func (u *Upstream) injectGlobalChatHeaders(req *http.Request, a *Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	req.Header.Set("X-No-Enterprise-Id", "1")
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// injectConversationHeaders injects the official client conversation family.
func (u *Upstream) injectConversationHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// A zero-value meta (direct ChatHeaders callers, tests) still needs an
		// aggregation key.
		convReqID = newMessageID()
	}
	messageID := newMessageID()
	if meta.ConversationID != "" {
		req.Header.Set("X-Conversation-ID", meta.ConversationID)
	}
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", convReqID)
	traceID := meta.TraceID
	if traceID == "" {
		traceID = convReqID
	}
	req.Header.Set("X-Trace-ID", traceID)
	b3Trace := convReqID
	if !validTraceID(b3Trace) {
		b3Trace = messageID
	}
	req.Header.Set("X-B3-TraceId", b3Trace)
	req.Header.Set("X-B3-SpanId", messageID[:16])
	req.Header.Set("X-B3-Sampled", "1")
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
func (u *Upstream) injectAttribution(req *http.Request) {
	name := u.attributionClientName()
	if name == "SaaS" {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", u.clientVersion())
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
	} else if ua := u.billingUA(); ua != "" {
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
