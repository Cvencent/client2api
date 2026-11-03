package kilocode

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// config.example.json is a deliverable: it must parse as this module's own
// config and stay usable as a copy-paste starting point (issue #147).  The
// example used to be checked by only five modules while twenty shipped one,
// and the one file whose shape was wrong sat in the unguarded fifteen: a
// copy-paste of it parsed to the zero config with no error anywhere.
//
// The zero-check is the point, not parsing: encoding/json ignores unknown
// keys, so a wrongly-shaped example "parses" into the zero config with
// err == nil and the module silently runs on defaults.  The example must
// therefore produce a config that is NOT the zero value.

// TestConfigExampleParsesToANonZeroConfig reads the example through the
// module's own parser and refuses the zero result.
func TestConfigExampleParsesToANonZeroConfig(t *testing.T) {
	raw, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatalf("reading config.example.json: %v", err)
	}
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("config.example.json does not parse as this module's config: %v", err)
	}
	if cfg.MaxTokensField == "" {
		t.Error("example decodes to the ZERO config: its keys matched no struct field " +
			"(issue #147's loomy shape) — a copy-paste start would silently run on defaults")
	}
}

// TestConfigExampleHasNoLiveCredentials keeps placeholder discipline across
// every example (issue #147's second finding): a credential field must be
// empty or carry an obvious placeholder.  URLs are not credentials (a token
// endpoint is public knowledge), self-documenting placeholders are the
// repo-wide convention (openrouter's sk-or-v1-REPLACE-ME, minimaxcode's
// mmoat_PUT-YOUR-ACCESS-TOKEN-HERE), and zcode's auth { mode: "api_key" }
// is a mode NAME, not a credential -- so the match is on the KEY itself,
// never on a line that merely mentions one.  What is refused is a
// specific-looking value: the loomy example shipped a 16-char id and a
// 32-char secret this way in the initial public release.
func TestConfigExampleHasNoLiveCredentials(t *testing.T) {
	raw, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatalf("reading config.example.json: %v", err)
	}
	// Credential-shaped keys, per module family.  Keys that merely name an
	// endpoint (token_url) or a header (max_tokens_field) are not here.
	keys := map[string]bool{"access_key_secret": true, "secret": true,
		"api_key": true, "password": true, "access_token": true,
		"refresh_token": true, "sms_token": true, "access_key_id": true}
	keyRe := regexp.MustCompile(`"([a-z_]+)"[[:space:]]*:`)
	for _, line := range strings.Split(string(raw), "\\n") {
		l := strings.ToLower(line)
		if strings.Contains(l, "_url") || strings.Contains(l, "endpoint") {
			continue // a URL is public knowledge, not a secret
		}
		km := keyRe.FindStringSubmatch(l)
		if km == nil || !keys[km[1]] {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		val := strings.Trim(strings.TrimSpace(line[idx+1:]), "\\")
		up := strings.ToUpper(val)
		// Self-documenting placeholders are fine; a value that looks real is not.
		if val == "" || strings.Contains(up, "REPLACE") || strings.Contains(up, "PUT-YOUR") ||
			strings.Contains(up, "YOUR_") || strings.Contains(up, "YOUR-") ||
			strings.Contains(up, "XXX") || strings.Contains(up, "EXAMPLE") {
			continue
		}
		if len(val) < 8 {
			continue // too short to be a real credential
		}
		t.Errorf("凭据字段 "+km[1]+" 带着一个具体值（长度 %d）：示例必须留空或用明显的占位符，真实值不能借示例文件进公共仓库（issue #147）: %s", len(val), strings.TrimSpace(line))
	}
}
