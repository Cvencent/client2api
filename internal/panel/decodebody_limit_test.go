package panel

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeBody 的请求上限回归（issue #78）：对话页每轮都把整段对话记录 PATCH
// 回来，长会话在旧的 1 MiB 上限处被静默截断，json.Unmarshal 报「unexpected
// end of JSON input」，前端又以 quiet 吞掉 400、随后重读把刚出现的回答抹掉。
// 上限提到 16 MiB，且越界必须是明确的 413（errBodyTooLarge），而不是一条
// 让人去查 JSON 语法的假错误。
func TestDecodeBodyTakesARealTranscript(t *testing.T) {
	// A ~2 MiB conversation: one huge user message plus the assistant reply.
	big := strings.Repeat("主要风险有三处，", 150000) // ~2.1 MB of CJK
	payload := `{"messages":[{"role":"user","content":"` + big + `"}]}`
	if len(payload) <= 1<<20 {
		t.Fatalf("fixture too small: %d bytes; the test must exceed the old 1 MiB limit", len(payload))
	}
	r := httptest.NewRequest("PATCH", "/panel/api/chat/conversations/x", bytes.NewBufferString(payload))
	var v struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := decodeBody(r, &v); err != nil {
		t.Fatalf("decodeBody rejected a real conversation of %d bytes: %v", len(payload), err)
	}
	if len(v.Messages) != 1 || len(v.Messages[0].Content) != len(big) {
		t.Fatalf("decoded %d message(s), content len %d; want 1 and %d", len(v.Messages), len(v.Messages[0].Content), len(big))
	}
}

func TestDecodeBodyNamesTheTruncation(t *testing.T) {
	// One byte past the cap: the old reader silently truncated this and the
	// caller answered 400 "unexpected end of JSON input".
	payload := bytes.Repeat([]byte("a"), maxPanelBody+64)
	r := httptest.NewRequest("PATCH", "/panel/api/chat/conversations/x", bytes.NewReader(payload))
	var v map[string]any
	err := decodeBody(r, &v)
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("decodeBody = %v, want errBodyTooLarge", err)
	}
	// A body at the cap still decodes (it is valid JSON of exactly that size).
	fit := `{"pad":"` + strings.Repeat("a", maxPanelBody-10) + `"}`
	r2 := httptest.NewRequest("PATCH", "/x", bytes.NewBufferString(fit))
	if err := decodeBody(r2, &v); err != nil {
		t.Fatalf("decodeBody rejected a body of exactly the cap: %v", err)
	}
}
