package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"client2api/internal/core"
)

// upstream.go is the HTTP half of the module: how a request is signed, sent,
// classified when it fails, and retried.
//
// It deliberately contains no protocol constants — those live next to the
// feature that uses them (sign.go, oauth.go, models.go, credits.go) — and no
// SSE parsing, which is in sse.go.

// ---------------------------------------------------------------------------
// error classification
// ---------------------------------------------------------------------------

// errKind is this module's own view of an upstream failure, before it is
// translated into a core.FailureKind.
type errKind int

const (
	kindNone errKind = iota
	kindNetwork
	kindAuth
	kindQuota
	kindQueue
	kindTransient
	kindClient
)

func (k errKind) String() string {
	switch k {
	case kindNone:
		return "none"
	case kindNetwork:
		return "network"
	case kindAuth:
		return "auth"
	case kindQuota:
		return "quota"
	case kindQueue:
		return "queue"
	case kindTransient:
		return "transient"
	case kindClient:
		return "client"
	default:
		return "unknown"
	}
}

// failureKind maps this module's classification onto the gateway's.
//
// kindQueue is reported as FailureRateLimited rather than FailureUpstream: the
// gateway's rotation policy treats a rate-limited failure as retryable, which
// is exactly what a concurrency-queue rejection is.
func failureKind(k errKind) core.FailureKind {
	switch k {
	case kindQuota:
		return core.FailureQuota
	case kindAuth:
		return core.FailureAuth
	case kindQueue, kindTransient, kindNetwork:
		return core.FailureRateLimited
	case kindClient:
		return core.FailureOther
	default:
		return core.FailureUpstream
	}
}

// Regexes the reference implementation uses to recognise failures that arrive
// with a status code that does not describe them.  CodeArts reports a
// concurrency-queue rejection as a 400 whose body names TM.00001041, and a
// per-minute token limit as an HTTP 200 with an SSE-embedded error_code, so
// the status alone is never enough.
var (
	reQueueBody = regexp.MustCompile(`(?i)peak\s+usage|try\s+again\s+after|peak\s+hours|high\s+demand|too\s+many\s+requests`)
	reAuthBody  = regexp.MustCompile(`(?i)invalid\s+token|token\s+expired|token\s+is\s+invalid`)
	reQueueCode = regexp.MustCompile(`(?i)81111|TPM|429|rate.?limit|too many requests|排队|限流`)
	reQuotaBody = regexp.MustCompile(`(?i)insufficient\s+(balance|quota|credit)|quota\s+(exceeded|exhausted)|out\s+of\s+(credit|quota)|not\s+enough\s+credit`)
	reCtxBody   = regexp.MustCompile(`(?i)context\s+length|maximum\s+context|too\s+long|exceeds?\s+the\s+maximum`)
)

// isQueueBody reports whether a 400 body describes the concurrency queue.
func isQueueBody(status int, body string) bool {
	if status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(body, "TM.00001041") || reQueueBody.MatchString(body)
}

// isAuthBody reports whether a response means the credential is no longer
// accepted.  The reference notes that APIG.0602 ("Invalid token") arrives as
// 401 and sometimes as 403, and that the security token can be revoked before
// its stated expiry, so the body is checked as well as the status.
func isAuthBody(status int, body string) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	return strings.Contains(body, "APIG.0602") || reAuthBody.MatchString(body)
}

// isQueueErrorCode reports whether an SSE-embedded error_code is the retryable
// queue/rate-limit family.
func isQueueErrorCode(code string) bool {
	if strings.TrimSpace(code) == "" {
		return false
	}
	return code == "TM.00001041" || reQueueCode.MatchString(code)
}

// classifyStatus turns a status code plus body into an errKind.
func classifyStatus(status int, body string) errKind {
	switch {
	case isAuthBody(status, body):
		return kindAuth
	case status == http.StatusPaymentRequired:
		return kindQuota
	case isQueueBody(status, body):
		return kindQueue
	case status == http.StatusTooManyRequests:
		return kindQuota
	case reQuotaBody.MatchString(body):
		return kindQuota
	case status >= 500:
		return kindTransient
	case status >= 400:
		return kindClient
	default:
		return kindNone
	}
}

// upstreamError is a classified upstream failure.
type upstreamError struct {
	Status     int
	Kind       errKind
	Message    string
	RetryAfter time.Duration
}

func (e *upstreamError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("codearts: upstream answered HTTP %d", e.Status)
}

// newUpstreamError builds one from a status and a body.
func newUpstreamError(status int, body string, header http.Header) *upstreamError {
	return &upstreamError{
		Status:     status,
		Kind:       classifyStatus(status, body),
		Message:    cleanErrorText(body),
		RetryAfter: parseRetryAfter(header),
	}
}

// asUpstreamError unwraps an *upstreamError out of an error chain.
func asUpstreamError(err error) (*upstreamError, bool) {
	var ue *upstreamError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// classifyErr reports an errKind for any error, so the pool can be told why a
// credential failed.
func classifyErr(err error) (errKind, string) {
	if err == nil {
		return kindNone, ""
	}
	if ue, ok := asUpstreamError(err); ok {
		return ue.Kind, ue.Message
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return kindTransient, err.Error()
	}
	return kindNetwork, err.Error()
}

// ---------------------------------------------------------------------------
// error text
// ---------------------------------------------------------------------------

const maxErrorText = 200

var (
	reTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	reWS      = regexp.MustCompile(`\s+`)
	reCredIn1 = regexp.MustCompile(`(?i)(secret_access_key|access_key_id|security_token|refresh_token)["'\s:=]+[A-Za-z0-9._~+/=-]{6,}`)
)

// cleanErrorText turns an upstream body into one short line fit for a log or a
// panel note: tags stripped, whitespace collapsed, credentials scrubbed, and
// the whole thing capped.
func cleanErrorText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = reTag.ReplaceAllString(s, " ")
	s = reWS.ReplaceAllString(s, " ")
	s = reCredIn1.ReplaceAllString(s, "$1=***")
	s = strings.TrimSpace(s)
	if len(s) > maxErrorText {
		s = strings.TrimSpace(s[:maxErrorText]) + "…"
	}
	return core.Redact(s)
}

// parseRetryAfter reads the several spellings of "wait this long" a gateway
// may send.  The result is capped so a hostile or buggy header cannot park an
// account for a week.
func parseRetryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	for _, name := range []string{"Retry-After", "X-RateLimit-Reset", "X-Ratelimit-Reset", "Retry-After-Ms"} {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		var d time.Duration
		if n, err := parseFloat(v); err == nil {
			if strings.HasSuffix(strings.ToLower(name), "-ms") {
				d = time.Duration(n) * time.Millisecond
			} else if n > 1e6 {
				// A unix timestamp rather than a delta.
				d = time.Until(time.Unix(int64(n), 0))
			} else {
				d = time.Duration(n * float64(time.Second))
			}
		} else if t, err := http.ParseTime(v); err == nil {
			d = time.Until(t)
		}
		if d > 0 {
			if d > 2*time.Hour {
				d = 2 * time.Hour
			}
			return d
		}
	}
	return 0
}

func parseFloat(s string) (float64, error) {
	var f float64
	err := json.Unmarshal([]byte(strings.TrimSpace(s)), &f)
	return f, err
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

// httpClient prefers the injected client, which is what every unit test uses
// and what the host wires a proxy or a TLS fingerprint onto.
func (c *Client) httpClient() *http.Client {
	if c.deps.HTTPClient != nil {
		return c.deps.HTTPClient
	}
	c.httpOnce.Do(func() {
		if c.httpLazy == nil {
			c.httpLazy = &http.Client{Timeout: 0}
		}
	})
	return c.httpLazy
}

// doSigned sends one request signed with `acct`, adding the unsigned headers
// the endpoint wants.
//
// `signedExtra` participates in the signature (the `maas_type` trap);
// `unsigned` is appended afterwards and must never be signed (`Agent-Type`,
// `X-Language`).
func (c *Client) doSigned(ctx context.Context, acct account, method, rawURL string, body []byte, signedExtra, unsigned map[string]string) (*http.Response, error) {
	signed, err := signRequest(acct.AccessKeyID, acct.SecretAccessKey, acct.SecurityToken, method, rawURL, body, signedExtra, time.Now())
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, fmt.Errorf("codearts: building the request: %w", err)
	}
	for k, v := range signedRequestHeaders(signed) {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range unsigned {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", c.cfg.userAgent())
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("codearts: calling %s: %w", rawURL, err)
	}
	return resp, nil
}

// readErrorBody drains a bounded amount of a failing response and turns it
// into a classified error.
func readErrorBody(resp *http.Response) *upstreamError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return newUpstreamError(resp.StatusCode, string(raw), resp.Header)
}

// doJSON sends a signed request, requires a 2xx, and decodes the body.
func (c *Client) doJSON(ctx context.Context, acct account, method, rawURL string, body []byte, signedExtra, unsigned map[string]string, out any) error {
	resp, err := c.doSigned(ctx, acct, method, rawURL, body, signedExtra, unsigned)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return readErrorBody(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("codearts: reading the response: %w", err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("codearts: the response is not the JSON it should be: %s", cleanErrorText(string(raw)))
	}
	return nil
}

// ---------------------------------------------------------------------------
// the chat request body
// ---------------------------------------------------------------------------

// roleAssistant is the one role whose history the vendor requires to carry a
// `reasoning_content` field; see wireMessage.
const roleAssistant = "assistant"

// wireMessage is one message as the chat endpoint expects it.
//
// The `reasoning_content` field is not optional in practice: deepseek-v4
// rejects an assistant history message that omits it with a 400 "Missing
// `reasoning_content` field".
//
// It is therefore a pointer that is always set on an assistant message — an
// empty string is a valid and required value — and left nil on every other
// role.  A plain string with `omitempty` would silently drop the empty case,
// which is exactly the shape the vendor refuses; a plain string without it
// would put a meaningless field on user and system messages.  Only assistant
// history is the vendor's complaint, so only assistant history is covered.
type wireMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	// name is only meaningful on a tool result, and the vendor ignores it
	// elsewhere, so it is omitted rather than always present.
	Name string `json:"name,omitempty"`
}

type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// wireTool is one tool definition, passed through unchanged from the gateway.
type wireTool struct {
	Type     string          `json:"type"`
	Function wireToolFuncDef `json:"function"`
}

type wireToolFuncDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// wireThinking pins the reasoning channel.
type wireThinking struct {
	Type string `json:"type"`
}

// chatBody is the exact JSON the chat endpoint receives.
//
// Field order matters only for readability here — encoding/json emits struct
// fields in declaration order — but the *presence* rules matter a great deal:
// `thinking` is sent only when the operator asked for reasoning to be off, and
// `tools` only when the caller supplied tools.
type chatBody struct {
	Model            string        `json:"model"`
	Messages         []wireMessage `json:"messages"`
	Stream           bool          `json:"stream"`
	PromptCacheKey   string        `json:"prompt_cache_key"`
	Include          []string      `json:"include"`
	ReasoningSummary string        `json:"reasoning_summary"`
	Thinking         *wireThinking `json:"thinking,omitempty"`
	ToolStream       bool          `json:"tool_stream"`
	MaxTokens        int           `json:"max_tokens"`
	Tools            []wireTool    `json:"tools,omitempty"`
	Temperature      *float64      `json:"temperature,omitempty"`
	TopP             *float64      `json:"top_p,omitempty"`
	Stop             []string      `json:"stop,omitempty"`
}

// buildChatBody renders the request the vendor's API expects.
//
// `sessionID` is this module's own per-request identifier: the reference sends
// it as both `prompt_cache_key` and the `Session-Id` header, which is what
// lets the backend reuse a prefix cache across the turns of one conversation.
func (c *Client) buildChatBody(req *core.ChatRequest, sessionID string) ([]byte, error) {
	msgs := make([]wireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		// Assistant history always carries the field, empty or not; see the
		// note on wireMessage.
		var reasoning *string
		if m.Role == roleAssistant {
			r := m.Reasoning
			reasoning = &r
		}
		msgs = append(msgs, wireMessage{
			Role:             m.Role,
			Content:          messageText(m),
			ReasoningContent: reasoning,
			ToolCalls:        wireToolCalls(m.ToolCalls),
			ToolCallID:       m.ToolCallID,
			Name:             m.Name,
		})
	}
	body := chatBody{
		Model:            req.Model,
		Messages:         msgs,
		Stream:           true,
		PromptCacheKey:   sessionID,
		Include:          []string{"reasoning.encrypted_content"},
		ReasoningSummary: "auto",
		ToolStream:       true,
		MaxTokens:        c.maxTokensFor(req),
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		Stop:             req.Stop,
	}
	if c.cfg.reasoningOff() {
		body.Thinking = &wireThinking{Type: "disabled"}
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, wireTool{
			Type: firstNonEmpty(t.Type, "function"),
			Function: wireToolFuncDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return json.Marshal(body)
}

// maxTokensFor resolves the output budget: the caller's own cap if it set one,
// otherwise the module's default.
//
// The caller's value is NOT clamped by the per-model budget.  A caller that
// asked for a specific number gets that number; the model budget is only what
// the gateway fills in when nobody asked.
func (c *Client) maxTokensFor(req *core.ChatRequest) int {
	if req != nil && req.MaxTokens != nil && *req.MaxTokens > 0 {
		return *req.MaxTokens
	}
	return c.cfg.maxTokens()
}

// messageText flattens a core.Message into the single string the vendor's API
// takes.  A message carrying ContentParts is joined; a plain Content is used
// as-is.
func messageText(m core.Message) string {
	if m.Content != "" {
		return m.Content
	}
	if len(m.Parts) == 0 {
		return ""
	}
	var b strings.Builder
	for i, p := range m.Parts {
		switch strings.ToLower(strings.TrimSpace(p.Type)) {
		case "", "text":
			if i > 0 && b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		case "image_url":
			// The chat endpoint is text-only; an image part is described
			// rather than silently dropped, so the model is not left guessing
			// why the conversation makes no sense.
			if i > 0 && b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("[image: ")
			b.WriteString(firstNonEmpty(p.ImageURL, "unavailable"))
			b.WriteString("]")
		}
	}
	return b.String()
}

// wireToolCalls converts the gateway's tool calls into the wire shape.
func wireToolCalls(calls []core.ToolCall) []wireToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]wireToolCall, 0, len(calls))
	for _, tc := range calls {
		out = append(out, wireToolCall{
			ID:   tc.ID,
			Type: firstNonEmpty(tc.Type, "function"),
			Function: wireToolFunction{
				Name:      tc.Name,
				Arguments: firstNonEmpty(tc.Arguments, "{}"),
			},
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// credential refresh
// ---------------------------------------------------------------------------

// refreshCredential renews one account's credential in place.
//
// It returns errRefreshTerminal when the vendor says the refresh token itself
// is dead, which is the one failure the pool treats as a reason to park the
// account indefinitely.
func (c *Client) refreshCredential(ctx context.Context, e *entry) error {
	acct := e.account()
	if !acct.refreshable() {
		return errors.New("codearts: this credential has no refresh token (or no stored DPoP key), so it cannot be renewed")
	}
	key, err := keyPairFromStoredJwk(acct.DpopPrivateJwk)
	if err != nil {
		return err
	}
	refreshed, err := c.exchangeRefreshToken(ctx, acct.RefreshToken, acct.CodeVerifier, key)
	if err != nil {
		return err
	}
	// The refresh answer does not repeat the identifying fields, so they are
	// carried over from the credential being renewed.
	refreshed.ID = acct.ID
	refreshed.Note = acct.Note
	refreshed.DomainID = firstNonEmpty(refreshed.DomainID, acct.DomainID)
	refreshed.UserID = firstNonEmpty(refreshed.UserID, acct.UserID)
	refreshed.UserName = firstNonEmpty(refreshed.UserName, acct.UserName)
	if refreshed.RefreshToken == "" {
		// A refresh that returns no new refresh token keeps the old one;
		// dropping it would silently make the account unrefreshable.
		refreshed.RefreshToken = acct.RefreshToken
	}
	c.pool.updateCredential(e, refreshed)
	return nil
}
