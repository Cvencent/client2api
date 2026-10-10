package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// gateway.go is the inference half of the module: the two /algo routes on
// gateway.qoder.com.cn that the desktop client's own agent uses.
//
//   - GET  /algo/api/v2/model/list        -- the model catalogue
//   - POST /algo/api/v2/service/pro/sse/agent_chat_generation -- the chat stream
//
// Both need the COSY signature (cosy.go); neither accepts the plain bearer
// token that the openapi host takes.  The route names and the SSE framing were
// read out of the CN desktop client's own bundle, not guessed, and both were
// confirmed live before this file was written.

const (
	// pathModelList is the vendor's catalogue, chat group included.
	pathModelList = pathAlgoPrefix + "/api/v2/model/list?Encode=1"
	// pathChat is the streamed completion route.  It is the same route the
	// sibling QoderWork product uses, which is why the SSE envelope in sse.go
	// matches it byte for byte.
	pathChat = pathAlgoPrefix + "/api/v2/service/pro/sse/agent_chat_generation"
)

// gatewayMaxJSON bounds a non-streaming gateway response body.
const gatewayMaxJSON = 4 << 20

// sessionEntry caches one account's signing material next to the token it was
// derived from, so a rotated token is never signed with a stale identity.
type sessionEntry struct {
	sess  cosySession
	token string
}

// gatewaySession returns the cached COSY session for an account, building one
// when it is missing or the token has changed.
func (c *Client) gatewaySession(acc account) (cosySession, error) {
	key := acc.ID
	c.sessMu.Lock()
	if c.sessions == nil {
		c.sessions = make(map[string]sessionEntry, maxCosySessions)
	}
	if e, ok := c.sessions[key]; ok && e.token == acc.Token {
		c.sessMu.Unlock()
		return e.sess, nil
	}
	c.sessMu.Unlock()

	sess, err := newCosySession(acc.UserID, acc.Name, "", acc.Token)
	if err != nil {
		return cosySession{}, err
	}

	c.sessMu.Lock()
	if len(c.sessions) >= maxCosySessions {
		c.sessions = make(map[string]sessionEntry, maxCosySessions)
	}
	c.sessions[key] = sessionEntry{sess: sess, token: acc.Token}
	c.sessMu.Unlock()
	return sess, nil
}

// forgetSession drops one account's signing material, so the next request
// rebuilds it against a freshly imported token.
func (c *Client) forgetSession(id string) {
	c.sessMu.Lock()
	delete(c.sessions, id)
	c.sessMu.Unlock()
}

// maxCosySessions caps the session cache.  Signing material is small, but the
// cache is rebuilt from scratch past the cap so an operator with a very large
// pool cannot grow it without bound.
const maxCosySessions = 256

// gatewayURL renders the absolute URL of one gateway path.
func (c *Client) gatewayURL(path string) string {
	return c.cfg.gatewayBase() + path
}

// signedRequest performs one signed gateway request and returns the raw
// response.  The caller owns the body.  method is GET or POST; body is the
// exact byte string that is both signed and sent.
func (c *Client) signedRequest(ctx context.Context, method, path, body, accept string, acc account) (*http.Response, error) {
	sess, err := c.gatewaySession(acc)
	if err != nil {
		return nil, err
	}
	rawURL := c.gatewayURL(path)
	hdrs, err := sess.headers(cosyRequest{
		UID:         acc.UserID,
		Body:        body,
		RawURL:      rawURL,
		Accept:      accept,
		UserAgent:   c.cfg.userAgent(),
		RequestID:   cosyRequestID(),
		XRequestID:  cosyRequestID(),
		UnixDate:    nowUnix(),
		CosyVersion: c.cfg.cosyVersion(),
		IDVersion:   c.cfg.cosyVersion(),
		ClientType:  fmt.Sprintf("%d", c.cfg.clientType()),
	})
	if err != nil {
		return nil, err
	}

	var reader io.Reader
	if method == http.MethodPost {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, fmt.Errorf("qoder: %s %s: %w", method, path, err)
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := c.gatewayHTTP().Do(req)
	if err != nil {
		return nil, fmt.Errorf("qoder: %s %s: %w", method, path, err)
	}
	return resp, nil
}

// gatewayHTTP is the client every gateway call goes through.
func (c *Client) gatewayHTTP() *http.Client {
	if c.up != nil && c.up.http != nil {
		return c.up.http
	}
	c.httpOnce.Do(func() {
		c.ownHTTP = &http.Client{Timeout: c.cfg.requestTimeout()}
	})
	return c.ownHTTP
}

// fetchModelList reads the vendor's catalogue with one account's signature.
func (c *Client) fetchModelList(ctx context.Context, acc account) ([]modelInfo, error) {
	resp, err := c.signedRequest(ctx, http.MethodGet, pathModelList, "", "application/json", acc)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, gatewayMaxJSON))
	if err != nil {
		return nil, fmt.Errorf("qoder: reading the model list: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, gatewayError(resp.StatusCode, payload)
	}
	var list gatewayModelList
	if err := json.Unmarshal(payload, &list); err != nil {
		return nil, fmt.Errorf("qoder: reading the model list: %w", err)
	}
	return list.chatModels(), nil
}

// openChat issues one signed chat request.  A non-2xx response is drained and
// turned into an error; on success the caller owns the response body.
func (c *Client) openChat(ctx context.Context, acc account, body string) (*http.Response, error) {
	resp, err := c.signedRequest(ctx, http.MethodPost, pathChat, body, "text/event-stream", acc)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, gatewayMaxJSON))
	resp.Body.Close()
	return nil, gatewayError(resp.StatusCode, payload)
}

// gatewayError renders a gateway failure as the module's apiError, which
// errors.go already classifies into a core.FailureKind.
func gatewayError(status int, payload []byte) error {
	code, message := decodeErrorMessage(payload)
	if message == "" {
		message = cleanErrorText(strings.TrimSpace(string(payload)))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return &apiError{Status: status, Code: code, Message: message}
}

// modelRefreshContext is the deadline one catalogue fetch runs under.
func (c *Client) modelRefreshContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, c.cfg.modelsTimeout())
}

// nowUTC is the clock the pool stamps its bookkeeping with.
func nowUTC() time.Time { return time.Now().UTC() }
