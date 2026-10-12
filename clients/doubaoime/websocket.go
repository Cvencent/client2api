package doubaoime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"client2api/internal/core"
	"golang.org/x/net/websocket"
)

// defaultAid is the Doubao Input Method app id carried in the registration
// query string and again in the WebSocket handshake.
const defaultAid = "401734"

func (c *Client) dialWebSocket(ctx context.Context, creds cachedCredentials) (*websocket.Conn, error) {
	raw := strings.TrimSpace(c.cfg.websocketURL())
	u, err := url.Parse(raw)
	if err != nil {
		return nil, core.Fail("doubaoime", "", core.FailureUpstream, http.StatusBadGateway,
			fmt.Errorf("doubaoime: invalid websocket URL: %s", core.Redact(err.Error())))
	}
	if u.Scheme != "wss" && u.Scheme != "ws" {
		return nil, core.Fail("doubaoime", "", core.FailureUpstream, http.StatusBadGateway,
			fmt.Errorf("doubaoime: websocket URL must use ws or wss, got %q", u.Scheme))
	}
	query := u.Query()
	query.Set("aid", defaultAid)
	if creds.DeviceID != "" {
		query.Set("device_id", creds.DeviceID)
	}
	u.RawQuery = query.Encode()

	header := http.Header{}
	header.Set("proto-version", "v2")
	header.Set("x-custom-keepalive", "true")
	header.Set("User-Agent", c.cfg.userAgent())

	cfg := &websocket.Config{
		Location: u,
		Origin:   &url.URL{Scheme: "https", Host: u.Host},
		Version:  websocket.ProtocolVersionHybi13,
		Header:   header,
		Dialer:   &net.Dialer{Timeout: c.cfg.requestTimeout()},
	}
	ws, err := cfg.DialContext(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, core.Fail("doubaoime", "", core.FailureUpstream, http.StatusBadGateway,
			fmt.Errorf("doubaoime: connecting to ASR websocket: %s", core.Redact(err.Error())))
	}
	ws.PayloadType = websocket.BinaryFrame
	ws.MaxPayloadBytes = 8 << 20
	return ws, nil
}

func (c *Client) send(ctx context.Context, ws *websocket.Conn, msg wsMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ws.SetWriteDeadline(time.Now().Add(c.cfg.requestTimeout())); err != nil {
		return core.Fail("doubaoime", "", core.FailureUpstream, http.StatusBadGateway,
			fmt.Errorf("doubaoime: websocket set deadline: %s", core.Redact(err.Error())))
	}
	if err := websocket.Message.Send(ws, marshalMessage(msg)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return core.Fail("doubaoime", "", core.FailureUpstream, http.StatusBadGateway,
			fmt.Errorf("doubaoime: websocket send: %s", core.Redact(err.Error())))
	}
	return nil
}

func (c *Client) receive(ctx context.Context, ws *websocket.Conn) (wsMessage, error) {
	if err := ctx.Err(); err != nil {
		return wsMessage{}, err
	}
	if err := ws.SetReadDeadline(time.Now().Add(c.cfg.resultTimeout())); err != nil {
		return wsMessage{}, core.Fail("doubaoime", "", core.FailureUpstream, http.StatusBadGateway,
			fmt.Errorf("doubaoime: websocket set deadline: %s", core.Redact(err.Error())))
	}
	var raw []byte
	if err := websocket.Message.Receive(ws, &raw); err != nil {
		if ctx.Err() != nil {
			return wsMessage{}, ctx.Err()
		}
		return wsMessage{}, core.Fail("doubaoime", "", core.FailureUpstream, http.StatusBadGateway,
			fmt.Errorf("doubaoime: websocket receive: %s", core.Redact(err.Error())))
	}
	return parseMessage(raw)
}

// waitFor sends req and reads frames until the expected lifecycle message
// (TaskStarted, SessionStarted, ...) arrives, surfacing an upstream error
// status instead of blocking until the deadline.
func (c *Client) waitFor(ctx context.Context, ws *websocket.Conn, requestID string, req wsMessage, expect string, timeout time.Duration) (wsMessage, error) {
	if err := c.send(ctx, ws, req); err != nil {
		return wsMessage{}, err
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return wsMessage{}, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return wsMessage{}, core.Fail("doubaoime", "", core.FailureUpstream, http.StatusGatewayTimeout,
				fmt.Errorf("doubaoime: timed out waiting for %s", expect))
		}
		waitCtx, cancel := context.WithTimeout(ctx, remaining)
		msg, err := c.receive(waitCtx, ws)
		cancel()
		if err != nil {
			return wsMessage{}, err
		}
		if msg.RequestID != "" && msg.RequestID != requestID {
			continue
		}
		if !statusOK(msg.StatusCode) {
			return wsMessage{}, classifyWSStatus(msg)
		}
		if strings.EqualFold(strings.TrimSpace(msg.MessageType), expect) {
			return msg, nil
		}
	}
}

// readFinal drains result frames until the session is finished, then merges
// every transcript segment seen along the way.
func (c *Client) readFinal(ctx context.Context, ws *websocket.Conn) (string, error) {
	deadline := time.Now().Add(c.cfg.resultTimeout())
	var segments []resultSegment
	finished := false
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		readCtx, cancel := context.WithTimeout(ctx, remaining)
		msg, err := c.receive(readCtx, ws)
		cancel()
		if err != nil {
			if isTimeoutError(err) {
				break
			}
			return "", err
		}
		if !statusOK(msg.StatusCode) {
			return "", classifyWSStatus(msg)
		}
		if strings.TrimSpace(msg.ResultJSON) != "" {
			if segs, err := parseResultJSON(msg.ResultJSON); err == nil {
				segments = append(segments, segs...)
			}
		}
		if isSessionFinished(msg.MessageType) {
			finished = true
			break
		}
	}
	if !finished && len(segments) == 0 {
		return "", core.Fail("doubaoime", "", core.FailureUpstream, http.StatusGatewayTimeout,
			errors.New("doubaoime: timed out waiting for transcription result"))
	}
	return mergeTranscript(segments)
}

func isSessionFinished(messageType string) bool {
	switch strings.ToLower(strings.TrimSpace(messageType)) {
	case "sessionfinished", "finishsession", "taskfinished", "done":
		return true
	default:
		return false
	}
}

// statusOK reports whether a frame carries the vendor's success status. The
// Doubao ASR service answers every lifecycle frame with code 20000000 (the
// ByteDance business "OK"), not an HTTP 200; a genuinely absent status is 0.
func statusOK(code int) bool {
	return code == 0 || code == 20000000
}

func classifyWSStatus(msg wsMessage) error {
	detail := strings.TrimSpace(msg.StatusMessage)
	if detail == "" {
		detail = strings.TrimSpace(msg.ResultJSON)
	}
	if len(detail) > 400 {
		detail = detail[:400]
	}
	kind := core.FailureUpstream
	switch msg.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = core.FailureAuth
	case http.StatusTooManyRequests:
		kind = core.FailureRateLimited
	}
	status := msg.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	return core.Fail("doubaoime", "", kind, status,
		fmt.Errorf("doubaoime: ASR upstream error %d: %s", msg.StatusCode, core.Redact(detail)))
}

func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "timeout")
}
