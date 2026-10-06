package openaicompat

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"client2api/internal/core"
)

// The companion services this module can connect to in one step.  Every field
// is data: the panel renders whatever QuickConnectTargets returns, so adding a
// second local gateway later is a new entry here rather than new panel code.
const (
	omniRouteID      = "omniroute"
	omniRouteBase    = "http://127.0.0.1:20128/v1"
	omniRouteConsole = "http://127.0.0.1:20128/dashboard/providers"
	omniRouteInstall = "npm i -g omniroute"
)

var _ core.QuickConnectProvider = (*Client)(nil)

// omniRouteTarget is OmniRoute's card.  The defaults mirror its documented
// zero-config install: the gateway listens on 127.0.0.1:20128, serves the
// OpenAI surface under /v1, and answers keyless for its own `auto` combo.
func omniRouteTarget() core.QuickConnectTarget {
	return core.QuickConnectTarget{
		ID:          omniRouteID,
		Label:       "OmniRoute",
		Help:        "本机运行的免费模型网关。连上以后，它聚合的来源、自动降级和模型路由都能从这里直接调用。",
		BaseURL:     omniRouteBase,
		Console:     omniRouteConsole,
		KeyOptional: true,
		Install:     omniRouteInstall,
	}
}

// quickConnectTarget resolves one target id.  Unknown ids are an error because
// reaching here with one means the panel sent something this build never
// advertised.
func quickConnectTarget(id string) (core.QuickConnectTarget, bool) {
	if strings.EqualFold(strings.TrimSpace(id), omniRouteID) {
		return omniRouteTarget(), true
	}
	return core.QuickConnectTarget{}, false
}

// QuickConnectTargets implements core.QuickConnectProvider.
func (c *Client) QuickConnectTargets(ctx context.Context) []core.QuickConnectTarget {
	return []core.QuickConnectTarget{omniRouteTarget()}
}

// ProbeQuickConnect implements core.QuickConnectProvider.  Any HTTP answer --
// including 401/403 -- proves the service is up, so only a transport failure
// counts as "not running".  That is deliberate: OmniRoute answers keyless on
// loopback, but a locked-down install may still demand a key, and the operator
// needs to be told to fetch one rather than told the service is missing.
func (c *Client) ProbeQuickConnect(ctx context.Context, id string) (core.QuickConnectStatus, error) {
	target, ok := quickConnectTarget(id)
	if !ok {
		return core.QuickConnectStatus{ID: id}, fmt.Errorf("unknown quick-connect target %q", id)
	}
	return c.probeQuickBase(ctx, target.ID, target.BaseURL), nil
}

// probeQuickBase is ProbeQuickConnect with the address supplied by the
// caller, so a test can aim it at a local listener instead of 20128.
func (c *Client) probeQuickBase(ctx context.Context, id, base string) core.QuickConnectStatus {
	st := core.QuickConnectStatus{ID: id}
	probeCtx, cancel := context.WithTimeout(ctxOrBackground(ctx), 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		st.Detail = truncate(core.Redact(err.Error()), 200)
		return st
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		st.Detail = "没有连上 " + base + "，确认它已经启动"
		return st
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can be reused; the body itself
	// is irrelevant, only the fact that something answered matters.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBytes))
	st.Running = true
	st.Detail = fmt.Sprintf("已响应（HTTP %d）", resp.StatusCode)
	return st
}

// ConnectQuickConnect implements core.QuickConnectProvider.  It goes through
// AddAccount on purpose: the result is the same panel-owned row the manual
// form would have produced, so routing, testing, disabling and removal all
// keep working without a second code path.
func (c *Client) ConnectQuickConnect(ctx context.Context, id string, fields map[string]string) (core.AccountRecord, error) {
	target, ok := quickConnectTarget(id)
	if !ok {
		return core.AccountRecord{}, fmt.Errorf("unknown quick-connect target %q", id)
	}
	base := strings.TrimRight(strings.TrimSpace(fields["base_url"]), "/")
	if base == "" {
		base = target.BaseURL
	}
	if u, err := url.Parse(base); err != nil || u.Hostname() == "" {
		return core.AccountRecord{}, fmt.Errorf("base URL %q is not a usable address", base)
	}
	// OmniRoute's documented zero-config entry point is the model id
	// "auto".  Seeding it here is what makes the post-connect test able to
	// run at all: without a model the probe has nothing to send.
	models := strings.TrimSpace(fields["models"])
	if models == "" {
		models = "auto"
	}
	return c.AddAccount(ctx, core.AccountSpec{
		ID:    target.ID,
		Label: firstNonEmpty(strings.TrimSpace(fields["label"]), target.Label),
		Fields: map[string]string{
			"provider": target.ID,
			"api_key":  fields["api_key"],
			"base_url": base,
			"models":   models,
		},
	})
}

// isLoopbackBase reports whether an upstream root points at this machine.
//
// Only such a provider may be stored without a credential.  A loopback address
// means the request never leaves the host, which is the one case where "no API
// key" describes the service (OmniRoute answers keyless on 127.0.0.1) instead
// of describing an operator who forgot to paste one.
func isLoopbackBase(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
