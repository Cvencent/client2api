package raccoon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTestAccountDisablesThinkingSoTheTinyProbeGetsContent pins the vendor
// behaviour that broke the live probe: SenseNova-6.8-Flash enables thinking by
// default, so a 16-token probe spends the entire budget on reasoning_content
// and never emits content. The probe must explicitly switch thinking off,
// otherwise the account is misreported as "answered without any text".
func TestTestAccountDisablesThinkingSoTheTinyProbeGetsContent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathChat {
			t.Errorf("unexpected path %q", r.URL.Path)
			return
		}
		var body struct {
			MaxTokens *int `json:"max_tokens"`
			ExtraBody struct {
				Thinking struct {
					Type string `json:"type"`
				} `json:"thinking"`
			} `json:"extra_body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if body.MaxTokens == nil || *body.MaxTokens != 16 {
			t.Errorf("max_tokens = %v, want the 16-token probe budget", body.MaxTokens)
		}
		if body.ExtraBody.Thinking.Type != "disabled" {
			// Mirror the live upstream: thinking wins the tiny budget and the
			// stream ends after reasoning without ever producing content.
			writeSSE(w,
				`{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
				`[DONE]`,
			)
			return
		}
		writeSSE(w,
			`{"choices":[{"delta":{"content":"pong"}}]}`,
			`{"choices":[{"finish_reason":"stop"}]}`,
			`[DONE]`,
		)
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(),
		fmt.Sprintf(`{"base_url":%q,"access_token":"tok","user_id":"u1"}`, ts.URL), ts.Client())
	res, err := c.TestAccount(context.Background(), "uid:u1")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount failed: %+v", res)
	}
	if res.Reply != "pong" {
		t.Fatalf("Reply = %q, want pong", res.Reply)
	}
}
