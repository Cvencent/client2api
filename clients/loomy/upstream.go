package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// upstream.go is the HTTP layer: one request builder for the business API, one
// for the account API, and the open of a chat stream.
//
// The two APIs differ in almost everything that matters -- different host,
// different auth, different response shape -- so they are kept apart rather
// than folded behind one helper that would have to branch on which one it is.

// maxResponseBytes bounds a non-streaming response body.  The model catalogue
// is the largest thing the vendor returns and it is a few tens of kilobytes.
const maxResponseBytes = 8 << 20

type upstream struct {
	cfg  config
	json *http.Client // bounded by a per-request timeout
	sse  *http.Client // no overall timeout: a stream lives as long as it lives
	logf func(format string, args ...any)
}

func (u *upstream) log(format string, args ...any) {
	if u.logf != nil {
		u.logf(format, args...)
	}
}

// pointsRecordsQuery is the read-only ledger query.  It is the cheapest request
// the vendor offers, which is why both the credential probe and the balance
// read use it.
func pointsRecordsQuery() []queryParam {
	return []queryParam{
		{Key: "pageNo", Value: "1"},
		{Key: "pageSize", Value: "1"},
		{Key: "recordType", Value: "all"},
	}
}

// business performs one JSON request against the business API.
//
// The business endpoints authenticate with the user's session in a lowercase
// `token` header and reject `Authorization` outright, so no Authorization is
// ever sent here -- that is the mirror image of the chat endpoint.
func (u *upstream) business(ctx context.Context, method, path string, params []queryParam, token string, body []byte) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, u.cfg.requestTimeout())
	defer cancel()

	url := u.cfg.apiBase() + path
	if query := buildEscapedQuery(params); query != "" {
		url += "?" + query
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("loomy: %s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := u.json.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loomy: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("loomy: reading %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &apiError{Status: resp.StatusCode, Message: truncate(strings.TrimSpace(string(payload)), 300)}
	}

	env := parseEnvelope(payload)
	if !env.OK {
		return nil, &apiError{Code: env.Code, Status: resp.StatusCode, Message: env.Message}
	}
	return env.Data, nil
}

// modelList reads the vendor's catalogue.
func (u *upstream) modelList(ctx context.Context, token string) ([]remoteModel, error) {
	ctx, cancel := context.WithTimeout(ctx, u.cfg.modelsTimeout())
	defer cancel()

	data, err := u.business(ctx, http.MethodGet, "/models", nil, token, nil)
	if err != nil {
		return nil, err
	}
	return parseRemoteModels(data)
}

// probeCredential verifies that a session is still accepted.
//
// It deliberately uses the cheapest read-only endpoint there is.  A transport
// failure is reported as a transport failure, never as an expired session:
// network jitter must not make the panel tell an operator to log in again.
func (u *upstream) probeCredential(ctx context.Context, token string) error {
	_, err := u.business(ctx, http.MethodGet, "/points/records", pointsRecordsQuery(), token, nil)
	return err
}

// ---------------------------------------------------------------------------
// Chat
// ---------------------------------------------------------------------------

// openChat opens a streaming completion.
//
// Both auth headers are sent on purpose.  /chat/completions is the only
// endpoint that accepts `Authorization: Bearer`, and the business endpoints are
// the only ones that accept `token`; sending both costs nothing and removes an
// entire class of "HTTP 200 with code 100002" confusion.  The `Bearer ` prefix
// is required -- the bare token is rejected even though the header is present.
func (u *upstream) openChat(ctx context.Context, token string, body []byte) (*http.Response, error) {
	url := u.cfg.apiBase() + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("loomy: chat: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("token", token)

	resp, err := u.sse.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loomy: chat: %w", err)
	}
	return resp, nil
}

// openChatStream opens a chat request and returns a live stream.
//
// A non-2xx status, or a 2xx whose body is a JSON business envelope rather than
// an SSE stream, is converted into a typed error here: by the time a caller has
// a Stream it is entitled to assume the upstream accepted the request.
func (u *upstream) openChatStream(ctx context.Context, token string, body []byte) (*chatStream, error) {
	// The request is bounded by chat_timeout, and the connection has its own,
	// much shorter first-byte deadline.  They are separate because the request
	// context has to stay alive for the whole stream while a stalled connection
	// must fail fast, so the first-byte deadline is a timer that is stopped as
	// soon as the response headers arrive.
	streamCtx, cancel := context.WithTimeout(ctx, u.cfg.chatTimeout())
	firstCtx, firstCancel := context.WithCancel(streamCtx)

	var firstByteFired atomic.Bool
	timer := time.AfterFunc(u.cfg.firstByteTimeout(), func() {
		firstByteFired.Store(true)
		firstCancel()
	})

	resp, err := u.openChat(firstCtx, token, body)
	timer.Stop()
	if err != nil {
		firstCancel()
		cancel()
		if firstByteFired.Load() {
			return nil, fmt.Errorf("loomy: chat: no response within %s", u.cfg.firstByteTimeout())
		}
		return nil, err
	}
	if firstByteFired.Load() {
		// The deadline fired in the instant between the headers arriving and the
		// timer being stopped, so the request context is already canceled and the
		// body cannot be read.  Report the stall rather than handing the caller a
		// stream that fails on its first read.
		_ = resp.Body.Close()
		firstCancel()
		cancel()
		return nil, fmt.Errorf("loomy: chat: no response within %s", u.cfg.firstByteTimeout())
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
		firstCancel()
		cancel()
		return nil, &apiError{
			Status:  resp.StatusCode,
			Message: truncate(strings.TrimSpace(string(payload)), 300),
		}
	}

	// A business failure can arrive as HTTP 200 with a JSON envelope instead of
	// a stream.  Detect it from the content type rather than from the first
	// bytes, so a stream is never consumed twice.
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
		firstCancel()
		cancel()
		env := parseEnvelope(payload)
		if !env.OK {
			return nil, &apiError{Code: env.Code, Status: resp.StatusCode, Message: env.Message}
		}
		return nil, fmt.Errorf("loomy: chat: the upstream answered %q instead of a stream", contentType)
	}

	// The headers are in, so the first-byte deadline has done its job and its
	// timer is already stopped.  firstCancel is deliberately NOT called here:
	// firstCtx is the context the request was issued with, so cancelling it would
	// abort the response body we are about to hand back.  firstCtx stays a live
	// child of streamCtx instead, which is what releases it -- Close calls
	// `cancel`, and chat_timeout cancels it even if nobody ever closes.
	//
	// The stream owns `cancel`: Close calls it, which is what releases the chat
	// timeout.
	return newChatStream(resp.Body, u.cfg.idleTimeout(), cancel), nil
}
