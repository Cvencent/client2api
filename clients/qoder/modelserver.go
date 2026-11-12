package qoder

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// modelserver.go is the international build's chat transport.
//
// The mainland build streams chat from the signed /algo edge
// (/algo/api/v2/service/pro/sse/agent_chat_generation, see gateway.go).  The
// international build does not: its desktop worker streams from a plain OpenAI
// endpoint instead --
//
//	https://<model host>/model/v1/chat/completions
//
// with `Authorization: Bearer <device token>`, no COSY signature, and the same
// OpenAI body the mainland edge accepts.  The model host is resolved by the
// vendor's own private HTTPDNS (the worker's QODER_MODEL_SERVER_DOMAIN is
// "api2-v2.qoder.sh", which has no public DNS record at all), so this transport
// takes its host from `global_model_server` and reports the host in its error
// message when the request cannot be made -- naming the real cause instead of
// pretending the credential is bad.
//
// Probing note: the international /algo edge is real (it publishes the model
// catalogue, and it accepts the COSY signature these very credentials mint), but
// it answers a chat request with "[FAIL]node:agent_router msg:None flow nodes
// found for router agent_router" because it expects the agent-flow envelope the
// worker builds, not a bare completion.  The clean endpoint is the one the
// vendor actually serves turns on, so that is what this file implements.

const (
	// pathModelServerChat is the international build's completion route.
	pathModelServerChat = "/model/v1/chat/completions"
	// modelServerMaxJSON bounds a non-streaming model-server error body.
	modelServerMaxJSON = 1 << 20
)

// openModelServerChat issues one OpenAI completion against the international
// model server.  modelKey is the bare catalogue id.
//
// The body is the same one the signed edge takes (see chatBody): the vendor's
// worker builds an identical object, so nothing is re-shaped here.
func (c *Client) openModelServerChat(ctx context.Context, acc account, body, modelKey string) (*http.Response, error) {
	base := c.cfg.modelServerBaseFor(acc.realmOf())
	if base == "" {
		return nil, fmt.Errorf("qoder: %s 没有配置可用的模型服务器地址", c.realmLabel(acc.realmOf()))
	}
	token := strings.TrimSpace(acc.Token)
	if token == "" {
		return nil, fmt.Errorf("qoder: 该账号没有设备令牌，无法调用模型服务器")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+pathModelServerChat, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("qoder: POST %s: %w", pathModelServerChat, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	// The worker's own client identity: a model server sees a request with no
	// request/session id as one it cannot attribute, and the vendor's telemetry
	// path records the gap.  Reusing the catalogue key as the session id keeps
	// one probe or turn self-consistent without inventing extra state.
	req.Header.Set("User-Agent", c.cfg.userAgent())
	req.Header.Set("X-Request-ID", cosyRequestID())
	if strings.TrimSpace(acc.ID) != "" {
		req.Header.Set("X-Session-ID", cosyRequestID())
	}

	resp, err := c.gatewayHTTP().Do(req)
	if err != nil {
		// Name the host: on a machine that cannot resolve the vendor's
		// HTTPDNS-only address this is the failure the operator will see, and
		// "dial tcp: no such host" alone would read as a bad credential.
		return nil, fmt.Errorf("qoder: 连接 %s 失败（该地址由厂商私有 DNS 解析，可能在本机不可达，请在平台配置里改 global_model_server）：%w", base, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, modelServerMaxJSON))
	resp.Body.Close()
	return nil, gatewayError(resp.StatusCode, payload)
}

// realmLabel renders one realm for an operator-facing message.
func (c *Client) realmLabel(realm string) string {
	if normalizeRealm(realm) == realmGlobal {
		return "国际版"
	}
	return "国内版"
}

// chatOpener returns the transport one account's realm uses.
//
// It exists so the chat loop and the probe both pick the transport in one place:
// the mainland build speaks the signed /algo route, the international build
// speaks the plain OpenAI model-server route.
func (c *Client) chatOpener(realm string) func(ctx context.Context, acc account, body, modelKey string) (*http.Response, error) {
	if normalizeRealm(realm) == realmGlobal {
		return func(ctx context.Context, acc account, body, modelKey string) (*http.Response, error) {
			return c.openModelServerChat(ctx, acc, body, modelKey)
		}
	}
	return func(ctx context.Context, acc account, body, modelKey string) (*http.Response, error) {
		return c.openChatFor(ctx, realm, acc, body, modelKey)
	}
}
