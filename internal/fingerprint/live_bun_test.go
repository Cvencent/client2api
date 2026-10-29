package fingerprint

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveZenWithBunProfile is a live probe against opencode.ai. Skipped unless
// CLIENT2API_LIVE_PROBE=1 so CI never hits the vendor.
func TestLiveZenWithBunProfile(t *testing.T) {
	if os.Getenv("CLIENT2API_LIVE_PROBE") != "1" {
		t.Skip("set CLIENT2API_LIVE_PROBE=1 to run the live probe")
	}
	c, err := New(Options{Profile: ProfileBun, Protocol: ProtocolH1, Timeout: 45 * time.Second, DialTimeout: 15 * time.Second})
	if true {
		// TEMPORARY experiment: pin to the first resolved IPv4 address
	}
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(c.CloseIdleConnections)

	body := map[string]any{
		"model": "glm-5.3-flash-free",
		"messages": []map[string]any{
			{"role": "system", "content": "<env>\n  Working directory: D:/client2api-src\n</env>"},
			{"role": "user", "content": "Reply with exactly: PONG"},
		},
		"stream":     true,
		"max_tokens": 8,
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{"name": "bash", "description": "Reserved for gateway client identification. Never call this tool.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}},
			{"type": "function", "function": map[string]any{"name": "read", "description": "Reserved for gateway client identification. Never call this tool.", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}},
		},
	}
	payload, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "https://opencode.ai/zen/v1/chat/completions", bytes.NewReader(payload))
	if os.Getenv("CLIENT2API_LIVE_PROBE_IP") != "" {
		// Pin the connection to the given address while keeping SNI+Host on opencode.ai.
		tr := c.Transport.(*http.Transport)
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(addr)
			_ = host
			return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp4", net.JoinHostPort(os.Getenv("CLIENT2API_LIVE_PROBE_IP"), port))
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-api-key", "public")
	req.Header.Set("User-Agent", "opencode/1.18.34")
	req.Header.Set("x-opencode-session", "ses_0123456789abcdef0123456789")
	req.Header.Set("x-opencode-session-id", "ses_0123456789abcdef0123456789")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
	t.Logf("HTTP %d | %s", resp.StatusCode, strings.ReplaceAll(string(buf), "\n", " "))
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 with the bun profile, got %d", resp.StatusCode)
	}
}
