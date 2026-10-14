package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// upstream transport
// ---------------------------------------------------------------------------

// identityHeadersFor returns the identity headers for one request.  The five
// constants are written verbatim; `includeAccept` is false for streaming
// requests (the vendor's own client sends none) and true for JSON reads.
func identityHeadersFor(token string, includeAccept bool) map[string]string {
	h := make(map[string]string, len(identityHeaders)+2)
	for k, v := range identityHeaders {
		h[k] = v
	}
	h["Authorization"] = "Bearer " + token
	h["Content-Type"] = "application/json"
	if includeAccept {
		h["Accept"] = "application/json"
	}
	return h
}

// applyExtraHeaders folds in the operator's extra headers without letting them
// override an identity or auth header.
func applyExtraHeaders(dst map[string]string, extra map[string]string) {
	for k, v := range extra {
		if _, exists := dst[k]; exists {
			continue
		}
		lower := strings.ToLower(k)
		if reservedHeader(lower) {
			continue
		}
		dst[k] = v
	}
}

// doJSON sends one JSON request and returns the status and body.
//
// It never sets a total timeout: streaming responses outlive any fixed budget,
// and the caller supplies a context that carries the deadline.
func (c *Client) doJSON(ctx context.Context, endpoint, path, token string, body any, includeAccept bool) (int, []byte, error) {
	payload, err := marshalRequest(body)
	if err != nil {
		return 0, nil, err
	}
	headers := identityHeadersFor(token, includeAccept)
	applyExtraHeaders(headers, c.cfg.ExtraHeaders)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, buf, nil
}

// marshalRequest serialises a value with alphabetical key order.  Every body
// this module sends goes through here so the byte order is uniform.
func marshalRequest(v any) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(v)
}

// maxErrorBody caps how much of a non-stream body is read.
const maxErrorBody = 4 << 20

// describeBody renders an upstream error body as one line.
func describeBody(status int, body []byte) string {
	msg := extractErrorMessage(body)
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	return truncate(fmt.Sprintf("HTTP %d: %s", status, msg), 400)
}

// extractErrorMessage digs the vendor's message out of its error envelope.
func extractErrorMessage(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	if env.Error.Message != "" {
		return env.Error.Message
	}
	return env.Message
}
