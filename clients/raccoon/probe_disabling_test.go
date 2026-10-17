package raccoon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
			MaxTokens       *int           `json:"max_tokens"`
			ReasoningEffort string         `json:"reasoning_effort"`
			ExtraBody       map[string]any `json:"extra_body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if body.MaxTokens == nil || *body.MaxTokens != 16 {
			t.Errorf("max_tokens = %v, want the 16-token probe budget", body.MaxTokens)
		}
		if body.ReasoningEffort != "none" || len(body.ExtraBody) != 0 {
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

func TestTestAccountAcceptsReasoningOnlyCompletion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathChat {
			t.Errorf("unexpected path %q", r.URL.Path)
			return
		}
		writeSSE(w,
			`{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
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
		t.Fatalf("a 200 completion carrying reasoning proves the credential works: %+v", res)
	}
	if res.Reply == "" {
		t.Fatal("successful reasoning-only probe must carry a non-empty result")
	}
}

func TestTestAccountRejectsAStreamErrorAfterReasoning(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathChat {
			t.Errorf("unexpected path %q", r.URL.Path)
			return
		}
		writeSSE(w,
			`{"choices":[{"delta":{"reasoning_content":"partial thought"}}]}`,
			`{"error":{"message":"upstream broke"}}`,
		)
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(),
		fmt.Sprintf(`{"base_url":%q,"access_token":"tok","user_id":"u1"}`, ts.URL), ts.Client())
	res, err := c.TestAccount(context.Background(), "uid:u1")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if res.OK {
		t.Fatal("reasoning received before a stream error must not prove the account works")
	}
	if !strings.Contains(res.Error, "upstream broke") {
		t.Fatalf("error = %q, want the real stream failure", res.Error)
	}
}

func TestTestAccountAcceptsCleanEmptyCompletion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathChat {
			t.Errorf("unexpected path %q", r.URL.Path)
			return
		}
		writeSSE(w,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
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
		t.Fatalf("a clean empty completion proves the credential works: %+v", res)
	}
	if res.Reply != "empty completion completed" {
		t.Fatalf("Reply = %q, want the explicit empty-completion evidence", res.Reply)
	}
}

func TestSuccessfulProbeRevivesADisabledAccount(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			`{"choices":[{"delta":{"content":"pong"}}]}`,
			`{"choices":[{"finish_reason":"stop"}]}`,
			`[DONE]`,
		)
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(),
		fmt.Sprintf(`{"base_url":%q,"access_token":"tok","user_id":"u1"}`, ts.URL), ts.Client())
	e := c.pool.find("uid:u1")
	if e == nil {
		t.Fatal("test account was not loaded")
	}
	c.pool.markDead(e, "old credential rejection")
	res, err := c.TestAccount(context.Background(), "uid:u1")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("an explicit test must succeed after the credential recovered: %+v", res)
	}
	after := c.pool.find("uid:u1")
	if after == nil {
		t.Fatal("account disappeared")
	}
	if after.acct.Disabled || after.state != stateReady || after.note != "" {
		t.Fatalf("successful probe left the account parked: disabled=%v state=%q note=%q",
			after.acct.Disabled, after.state, after.note)
	}
}
