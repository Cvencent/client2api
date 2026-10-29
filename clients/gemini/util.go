package gemini

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// firstNonEmpty returns the first non-blank value.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// truncate shortens s to at most n runes, appending an ellipsis when it cut
// something.  Rune-based, because vendor messages may be Chinese.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// clientPath joins the module's data dir with a store filename.
func clientPath(dataDir, name string) string {
	if dataDir == "" {
		return name
	}
	return filepath.Join(dataDir, name)
}

// randomHex returns n random bytes as a hex string.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b)
}

// rawJSON returns a RawMessage that is exactly the given literal.
func rawJSON(literal string) json.RawMessage { return json.RawMessage(literal) }

// jsonUnmarshal decodes without the caller importing encoding/json for a
// one-off struct.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// maskSecret renders a credential, its length and an 8-character prefix.  It
// is a local copy so the module does not depend on internal/core for a log
// line.
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "***"
	}
	return s[:8] + "…"
}

// newTransport builds the module's own transport, honouring the configured
// proxy.  A chat stream can idle for a long time between deltas, so the pool is
// kept warm rather than relying on the default.
func newTransport(proxy string) *http.Transport {
	tr := &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return tr
}
