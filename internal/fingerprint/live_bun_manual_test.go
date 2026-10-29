package fingerprint

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// utlsWrap wraps the raw connection and applies the preset.
func utlsWrap(raw net.Conn) *utls.UConn {
	u := utls.UClient(raw, &utls.Config{ServerName: "opencode.ai"}, utls.HelloCustom)
	_ = u.ApplyPreset(bunSpec(true))
	return u
}

// TestLiveZenManualRequest writes the HTTP/1.1 request by hand over the uTLS
// connection, in Bun's exact header order, so a JA4H-style HTTP-layer rule is
// exercised too. Skipped unless CLIENT2API_LIVE_PROBE=1.
func TestLiveZenManualRequest(t *testing.T) {
	if os.Getenv("CLIENT2API_LIVE_PROBE") != "1" {
		t.Skip("set CLIENT2API_LIVE_PROBE=1 to run the live probe")
	}
	d := &net.Dialer{Timeout: 15 * time.Second}
	dialTarget := "opencode.ai:443"
	if ip := os.Getenv("OC_IP"); ip != "" {
		dialTarget = "[" + ip + "]:443"
	}
	raw, err := d.DialContext(context.Background(), "tcp", dialTarget)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	uconn := utlsWrap(raw)
	if err := uconn.HandshakeContext(context.Background()); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	t.Logf("negotiated: %s", tls.VersionName(uconn.ConnectionState().Version))

	sess := "ses_0123456789abcdef0123456789"
	body := `{"model":"glm-5.3-flash-free","messages":[{"role":"system","content":"<env>\n  Working directory: D:/client2api-src\n</env>"},{"role":"user","content":"Reply with exactly: PONG"}],"stream":true,"max_tokens":8,"tools":[{"type":"function","function":{"name":"bash","description":"Reserved for gateway client identification. Never call this tool.","parameters":{"type":"object","properties":{}}}},{"type":"function","function":{"name":"read","description":"Reserved for gateway client identification. Never call this tool.","parameters":{"type":"object","properties":{}}}}]}`
	req := strings.Join([]string{
		"POST /zen/v1/chat/completions HTTP/1.1",
		"Authorization: Bearer public",
		"Content-Type: application/json",
		"User-Agent: opencode/1.18.34",
		"x-opencode-session: " + sess,
		"x-opencode-session-id: " + sess,
		"Connection: keep-alive",
		"Accept: */*",
		"Host: opencode.ai",
		"Accept-Encoding: gzip, deflate, br, zstd",
		"Content-Length: " + strconv.Itoa(len(body)),
		"",
		"",
	}, "\r\n")
	if _, err := uconn.Write([]byte(req + body)); err != nil {
		t.Fatalf("write: %v", err)
	}
	uconn.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(uconn)
	status, _ := br.ReadString('\n')
	t.Logf("status line: %s", strings.TrimSpace(status))
	head := make([]byte, 0, 1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		head = append(head, line...)
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	t.Logf("headers: %s", string(head))
	chunk, _ := br.ReadString('\n')
	t.Logf("first body line: %s", strings.TrimSpace(chunk))
}
