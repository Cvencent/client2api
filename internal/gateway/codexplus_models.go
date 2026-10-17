package gateway

import (
	"fmt"
	"net/http"
	"strings"
)

// codexPlusModelRequest reports whether the catalogue request comes from
// Codex++'s "fetch from upstream" action.  That client keeps only the model id
// from /v1/models, then parses its own [1M]/[272K] suffix into the per-model
// context window.  Keeping this compatibility behind the client's user agent
// leaves the ordinary OpenAI catalogue unchanged.
func codexPlusModelRequest(r *http.Request) bool {
	ua := strings.TrimSpace(r.Header.Get("User-Agent"))
	return strings.HasPrefix(strings.ToLower(ua), "codexplusplus/")
}

// codexPlusWindowSuffix renders a token count in the syntax Codex++ already
// understands: K/M when exact, otherwise the raw integer.
func codexPlusWindowSuffix(tokens int64) string {
	switch {
	case tokens > 0 && tokens%1_000_000 == 0:
		return fmt.Sprintf("%dM", tokens/1_000_000)
	case tokens > 0 && tokens%1_000 == 0:
		return fmt.Sprintf("%dK", tokens/1_000)
	default:
		return fmt.Sprintf("%d", tokens)
	}
}

// withCodexPlusWindowSuffix adds the context window to an advertised id.
func withCodexPlusWindowSuffix(id string, tokens int64) string {
	if id == "" || tokens <= 0 {
		return id
	}
	close := strings.LastIndexByte(id, ']')
	if close == len(id)-1 {
		if open := strings.LastIndexByte(id[:close], '['); open >= 0 {
			return id
		}
	}
	return id + "[" + codexPlusWindowSuffix(tokens) + "]"
}
