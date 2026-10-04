package minimaxcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// fakeTransport is an http.RoundTripper that records what was sent and replies
// from a scripted handler.  No test in this file touches the network.
type fakeTransport struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	handler  func(*http.Request) (*http.Response, error)
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		body = string(b)
		req.Body = io.NopCloser(strings.NewReader(body))
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.bodies = append(f.bodies, body)
	handler := f.handler
	f.mu.Unlock()

	if handler == nil {
		return nil, errors.New("fakeTransport: no handler installed")
	}
	return handler(req)
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeTransport) requestAt(i int) *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.requests) {
		return nil
	}
	return f.requests[i]
}

func (f *fakeTransport) bodyAt(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.bodies) {
		return ""
	}
	return f.bodies[i]
}

func newResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    &http.Request{},
	}
}

func jsonResponse(status int, body string) *http.Response {
	return newResponse(status, "application/json", body)
}

// newTestClient builds a Client whose whole world is a temporary directory and
// a scripted transport.  Discovery is off unless the caller turns it on, so a
// developer's own MiniMax Code sign-in can never leak into a test.
func newTestClient(t *testing.T, cfg map[string]any, handler func(*http.Request) (*http.Response, error)) (*Client, *fakeTransport) {
	t.Helper()
	if cfg == nil {
		cfg = map[string]any{}
	}
	if _, ok := cfg["auto_discover"]; !ok {
		cfg["auto_discover"] = false
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal test config: %v", err)
	}

	ft := &fakeTransport{handler: handler}
	built, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(raw),
		HTTPClient: &http.Client{Transport: ft},
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := built.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", built)
	}
	return c, ft
}

const testToken = "mmoat_test_token_0001"

func oneAccount() map[string]any {
	return map[string]any{
		"accounts": []map[string]any{
			{"id": "acct-1", "label": "Test account", "access_token": testToken},
		},
	}
}

// reply is the vendor's own non-streaming response, captured verbatim from the
// desktop client (see the package comment).  The tests assert against it rather
// than against a convenient invention.
const reply = `{"id":"070b10a36ea5867a0ae68d0d587eb546","type":"message","role":"assistant",` +
	`"model":"MiniMax-M3","content":[{"text":"Hey, hello there!","type":"text"}],` +
	`"stop_reason":"end_turn","usage":{"input_tokens":41,"output_tokens":6,` +
	`"cache_creation_input_tokens":0,"cache_read_input_tokens":128,"service_tier":"standard"},` +
	`"base_resp":{"status_code":0,"status_msg":""}}`

// collected is what a drained stream produced.
type collected struct {
	text      string
	reasoning string
	finish    string
	usage     *core.Usage
	events    int
}

func drain(t *testing.T, s core.Stream) collected {
	t.Helper()
	if s == nil {
		t.Fatal("drain: nil stream")
	}
	defer s.Close()

	var out collected
	for i := 0; i < 5000; i++ {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		out.events++
		switch ev.Type {
		case core.EventDelta:
			out.text += ev.Delta
			out.reasoning += ev.Reasoning
		case core.EventUsage:
			out.usage = ev.Usage
		case core.EventDone:
			out.finish = ev.Finish
		case core.EventError:
			t.Fatalf("stream reported an error: %v", ev.Err)
		}
	}
	t.Fatal("stream never reached EOF")
	return out
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// the happy paths
// ---------------------------------------------------------------------------

func TestChatSpeaksAnthropicMessagesAndAnswersOpenAI(t *testing.T) {
	c, ft := newTestClient(t, oneAccount(), func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, reply), nil
	})

	maxTokens := 32
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:     "MiniMax-M3",
		MaxTokens: &maxTokens,
		Messages:  []core.Message{{Role: "user", Content: "say hi in 3 words"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := drain(t, stream)

	if got.text != "Hey, hello there!" {
		t.Fatalf("text = %q, want %q", got.text, "Hey, hello there!")
	}
	if got.finish != "stop" {
		t.Fatalf("finish = %q, want stop", got.finish)
	}
	if got.usage == nil {
		t.Fatal("no usage event")
	}
	// 41 input + 128 cache-read + 0 cache-creation.
	if got.usage.PromptTokens != 169 || got.usage.CompletionTokens != 6 || got.usage.TotalTokens != 175 {
		t.Fatalf("usage = %+v, want prompt 169 / completion 6 / total 175", *got.usage)
	}
	if got.usage.CachedTokens != 128 {
		t.Fatalf("cached = %d, want 128", got.usage.CachedTokens)
	}

	if ft.count() != 1 {
		t.Fatalf("upstream saw %d requests, want 1", ft.count())
	}
	req := ft.requestAt(0)
	if req.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", req.Method)
	}
	if req.URL.Path != "/mavis/api/v1/llm/v1/messages" {
		t.Fatalf("path = %q, want the captured endpoint", req.URL.Path)
	}
	if req.URL.Host != "agent.minimax.cn" {
		t.Fatalf("host = %q, want agent.minimax.cn", req.URL.Host)
	}

	// Exactly the headers the capture showed, and nothing else credential-shaped.
	if got := req.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("anthropic-version"); got != anthropicVersion {
		t.Fatalf("anthropic-version = %q, want %q", got, anthropicVersion)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Fatalf("the module sent x-api-key (%q); the capture never showed one", got)
	}

	body := ft.bodyAt(0)
	var sent struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
		Messages  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("request body is not JSON (%v): %s", err, body)
	}
	if sent.Model != "MiniMax-M3" {
		t.Fatalf("model = %q", sent.Model)
	}
	if sent.MaxTokens != 32 {
		t.Fatalf("max_tokens = %d, want 32", sent.MaxTokens)
	}
	if sent.Stream {
		t.Fatal("a non-streaming request set stream:true")
	}
	if len(sent.Messages) != 1 || sent.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v", sent.Messages)
	}
	// Anthropic accepts either a bare string or a block array here; the module
	// normalises to blocks, so that is what the wire must carry.
	blocks := sent.Messages[0].Content
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "say hi in 3 words" {
		t.Fatalf("content blocks = %+v, want one text block carrying the prompt", blocks)
	}
}

func TestChatTranslatesAnthropicSSE(t *testing.T) {
	const frames = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"MiniMax-M3","usage":{"input_tokens":41,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hey"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", hello!"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}

event: message_stop
data: {"type":"message_stop"}

`

	c, _ := newTestClient(t, oneAccount(), func(*http.Request) (*http.Response, error) {
		return newResponse(200, "text/event-stream", frames), nil
	})

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "MiniMax-M3",
		Stream:   true,
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := drain(t, stream)

	if got.text != "Hey, hello!" {
		t.Fatalf("text = %q, want %q", got.text, "Hey, hello!")
	}
	if got.finish != "stop" {
		t.Fatalf("finish = %q, want stop", got.finish)
	}
	if got.usage == nil || got.usage.CompletionTokens != 6 {
		t.Fatalf("usage = %+v, want completion 6", got.usage)
	}
}

func TestChatWithoutAnAccountReportsNotConfigured(t *testing.T) {
	c, ft := newTestClient(t, nil, nil)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "MiniMax-M3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want core.ErrNotConfigured", err)
	}
	if ft.count() != 0 {
		t.Fatalf("an unconfigured module still made %d upstream calls", ft.count())
	}
}

func TestChatRejectsARequestWithNoModel(t *testing.T) {
	c, _ := newTestClient(t, oneAccount(), nil)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want core.ErrUnsupported", err)
	}
}

// ---------------------------------------------------------------------------
// failure classification
// ---------------------------------------------------------------------------

func TestStatusCodesMapToCoreFailureKinds(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   core.FailureKind
	}{
		{"401 token required", 401, `{"code":401,"message":"token is required"}`, core.FailureAuth},
		{"402 needs credit", 402, `{"message":"insufficient balance, please recharge"}`, core.FailureQuota},
		{"403 risk control", 403, `{"message":"request has been blocked due to unusual activity"}`, core.FailureWAF},
		{"403 plain refusal", 403, `{"message":"this account is not permitted"}`, core.FailureAuth},
		{"429 too many requests", 429, `{"message":"too many requests"}`, core.FailureRateLimited},
		{"503 unavailable", 503, `{"message":"service unavailable"}`, core.FailureUpstream},
		{"400 unknown model", 400, `{"message":"invalid model: MiniMax-Nope"}`, core.FailureOther},
		{"400 no route", 400, `{"errorCode":50115,"errorReason":"direct_route_not_configured"}`, core.FailureOther},
		{"400 malformed request", 400, `{"message":"max_tokens must be positive"}`, core.FailureOther},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, oneAccount(), func(*http.Request) (*http.Response, error) {
				return jsonResponse(tc.status, tc.body), nil
			})

			_, err := c.Chat(context.Background(), &core.ChatRequest{
				Model:    "MiniMax-M3",
				Messages: []core.Message{{Role: "user", Content: "hi"}},
			})
			if err == nil {
				t.Fatal("Chat succeeded, want a failure")
			}
			f, ok := core.AsFailure(err)
			if !ok {
				t.Fatalf("error is %T (%v), not a *core.Failure", err, err)
			}
			if f.Kind != tc.want {
				t.Fatalf("kind = %q, want %q (error: %v)", f.Kind, tc.want, err)
			}
			if f.Client != name {
				t.Fatalf("client = %q, want %q", f.Client, name)
			}
			if f.Account != "acct-1" {
				t.Fatalf("account = %q, want acct-1", f.Account)
			}
			if !strings.Contains(err.Error(), "HTTP") {
				t.Fatalf("the vendor's status is missing from %q", err.Error())
			}
		})
	}
}

func TestAnErrorInsideHTTP200IsStillAFailure(t *testing.T) {
	cases := []struct {
		name string
		body string
		want core.FailureKind
	}{
		{
			name: "base_resp quota",
			body: `{"base_resp":{"status_code":1002,"status_msg":"insufficient balance"}}`,
			want: core.FailureQuota,
		},
		{
			name: "base_resp auth",
			body: `{"base_resp":{"status_code":401,"status_msg":"token is required"}}`,
			want: core.FailureAuth,
		},
		{
			name: "base_resp risk",
			body: `{"base_resp":{"status_code":1006,"status_msg":"request has been blocked due to unusual activity"}}`,
			want: core.FailureWAF,
		},
		{
			name: "anthropic error member",
			body: `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`,
			want: core.FailureOther,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, oneAccount(), func(*http.Request) (*http.Response, error) {
				return jsonResponse(200, tc.body), nil
			})

			_, err := c.Chat(context.Background(), &core.ChatRequest{
				Model:    "MiniMax-M3",
				Messages: []core.Message{{Role: "user", Content: "hi"}},
			})
			if err == nil {
				t.Fatal("Chat returned a stream for an in-body error")
			}
			f, ok := core.AsFailure(err)
			if !ok {
				t.Fatalf("error is %T (%v), not a *core.Failure", err, err)
			}
			if f.Kind != tc.want {
				t.Fatalf("kind = %q, want %q (error: %v)", f.Kind, tc.want, err)
			}
		})
	}
}

func TestARefusedCredentialIsNotRetriedOnItself(t *testing.T) {
	c, ft := newTestClient(t, oneAccount(), func(*http.Request) (*http.Response, error) {
		return jsonResponse(401, `{"message":"token is required"}`), nil
	})

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "MiniMax-M3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat succeeded with a refused credential")
	}
	// One account, one attempt: the row is parked, not hammered.
	if ft.count() != 1 {
		t.Fatalf("upstream saw %d attempts, want exactly 1", ft.count())
	}

	status := c.Status(context.Background())
	if status.Ready {
		t.Fatal("Status reported ready with a parked credential")
	}
	if len(status.Accounts) != 1 || status.Accounts[0].State != stateInvalid {
		t.Fatalf("accounts = %+v, want one invalid row", status.Accounts)
	}
	if !strings.Contains(status.Detail, "invalid=1") {
		t.Fatalf("detail = %q, want it to name the invalid account", status.Detail)
	}
}

func TestAModelRejectionDoesNotRotateToAnotherAccount(t *testing.T) {
	c, ft := newTestClient(t, map[string]any{
		"accounts": []map[string]any{
			{"id": "acct-1", "access_token": "mmoat_one"},
			{"id": "acct-2", "access_token": "mmoat_two"},
		},
	}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(400, `{"message":"invalid model: MiniMax-Nope"}`), nil
	})

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "MiniMax-Nope",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat succeeded with an unroutable model")
	}
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want it to wrap core.ErrUnsupported", err)
	}
	if f, ok := core.AsFailure(err); !ok || f.Kind != core.FailureOther {
		t.Fatalf("kind = %v, want FailureOther (the kind the gateway never rotates)", err)
	}
	if ft.count() != 1 {
		t.Fatalf("upstream saw %d attempts; a model rejection must not be retried on another account", ft.count())
	}
}

func TestARejectedCredentialNeverLeaksItsToken(t *testing.T) {
	const secret = "mmoat_do_not_log_me_4711"
	c, _ := newTestClient(t, map[string]any{
		"accounts": []map[string]any{{"id": "acct-1", "access_token": secret}},
	}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(401, `{"message":"token `+secret+` is not valid"}`), nil
	})

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "MiniMax-M3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the credential appears in the error: %v", err)
	}

	rows, rerr := c.Accounts(context.Background())
	if rerr != nil {
		t.Fatalf("Accounts: %v", rerr)
	}
	blob, _ := json.Marshal(rows)
	if strings.Contains(string(blob), secret) {
		t.Fatalf("the credential appears in the account table: %s", blob)
	}
}

// ---------------------------------------------------------------------------
// discovery and the account table
// ---------------------------------------------------------------------------

const authFixture = `{"schemaVersion":1,"records":{` +
	`"user\u0000mcode-public":{"schemaVersion":1,"accessToken":"mmoat_discovered_9001",` +
	`"refreshToken":"mmort_refresh_9001","tokenType":"Bearer","clientId":"mcode-public",` +
	`"scopes":["openid"],"audience":"agent","expiresAtMs":4102444800000,` +
	`"generation":3,"loginEpoch":"epoch-1"}}}`

const authStateFixture = `{"schemaVersion":1,"status":"authenticated","storeKind":"file",` +
	`"clientId":"mcode-public","buildEnv":"cn","region":"prod",` +
	`"generation":3,"expiresAtMs":4102444800000}`

// desktopStore writes a credential store that looks like the one the desktop
// client leaves behind, and returns the auth.json path.
func desktopStore(t *testing.T) (root, authPath string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "auth")
	dir := filepath.Join(root, "cn", "prod", "mcode-public")
	authPath = filepath.Join(dir, "auth.json")
	writeFile(t, authPath, authFixture)
	writeFile(t, filepath.Join(dir, "auth-state.json"), authStateFixture)
	return root, authPath
}

func TestDiscoveryReadsTheDesktopStoreWithoutLeakingTheToken(t *testing.T) {
	root, authPath := desktopStore(t)

	c, ft := newTestClient(t, map[string]any{"auto_discover": true, "auth_dir": root}, nil)

	rows, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want exactly 1", rows)
	}
	if rows[0].ID != "minimaxcode:cn/prod/mcode-public" {
		t.Fatalf("id = %q, want minimaxcode:cn/prod/mcode-public", rows[0].ID)
	}
	if !rows[0].Enabled {
		t.Fatal("a freshly discovered credential should be enabled")
	}

	blob, _ := json.Marshal(rows)
	if strings.Contains(string(blob), "mmoat_discovered_9001") {
		t.Fatalf("the access token is in the account table: %s", blob)
	}
	if strings.Contains(string(blob), "mmort_refresh_9001") {
		t.Fatalf("the refresh token is in the account table: %s", blob)
	}
	if ft.count() != 0 {
		t.Fatalf("listing accounts made %d network calls", ft.count())
	}

	// The id is derived from where the credential lives, so a second process
	// sees the same row after a restart.
	c2, _ := newTestClient(t, map[string]any{"auto_discover": true, "auth_dir": root}, nil)
	rows2, err := c2.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts (second client): %v", err)
	}
	if len(rows2) != 1 || rows2[0].ID != rows[0].ID {
		t.Fatalf("the row id is not stable across processes: %+v vs %+v", rows2, rows)
	}

	// And it is offered for import, pointing at the file it came from.
	found, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("Discover = %+v, want one entry", found)
	}
	if found[0].Path != authPath {
		t.Fatalf("path = %q, want %q", found[0].Path, authPath)
	}
	if !found[0].Importable {
		t.Fatal("the credential was not offered as importable")
	}
}

func TestDiscoverySkipsAStoreWithNoUsableToken(t *testing.T) {
	root := filepath.Join(t.TempDir(), "auth")
	dir := filepath.Join(root, "cn", "prod", "mcode-public")
	writeFile(t, filepath.Join(dir, "auth.json"), `{"schemaVersion":1,"records":{}}`)
	writeFile(t, filepath.Join(dir, "auth-state.json"), authStateFixture)

	c, _ := newTestClient(t, map[string]any{"auto_discover": true, "auth_dir": root}, nil)
	rows, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want none: the store carries no token", rows)
	}
}

func TestImportCopiesTheCredentialAndLeavesTheDesktopFileAlone(t *testing.T) {
	root, authPath := desktopStore(t)
	before, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	c, _ := newTestClient(t, map[string]any{"auto_discover": true, "auth_dir": root}, nil)

	imported, err := c.Import(context.Background(), []string{authPath}, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(imported) != 1 {
		t.Fatalf("Import returned %+v, want one row", imported)
	}
	if imported[0].Fields["source"] != sourceManaged {
		t.Fatalf("source = %v, want %q", imported[0].Fields["source"], sourceManaged)
	}

	after, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("re-read fixture: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the desktop client's store was modified:\n before: %s\n after:  %s", before, after)
	}

	// Importing something that is not there is an error, not a silent no-op.
	if _, err := c.Import(context.Background(), []string{filepath.Join(root, "nope", "auth.json")}, false); err == nil {
		t.Fatal("Import accepted a path it cannot see")
	}
}

func TestAddAccountNeedsACredentialAndRemoveHidesADiscoveredRow(t *testing.T) {
	root, _ := desktopStore(t)
	c, _ := newTestClient(t, map[string]any{"auto_discover": true, "auth_dir": root}, nil)
	ctx := context.Background()

	if _, err := c.AddAccount(ctx, core.AccountSpec{Fields: map[string]string{}}); err == nil {
		t.Fatal("AddAccount accepted an empty access_token")
	}
	if _, err := c.AddAccount(ctx, core.AccountSpec{Fields: map[string]string{
		"access_token": "mmoat_x",
		"base_url":     "ftp://nope",
	}}); err == nil {
		t.Fatal("AddAccount accepted a base_url that is not http(s)")
	}

	added, err := c.AddAccount(ctx, core.AccountSpec{
		Fields: map[string]string{"access_token": "mmoat_managed_0001", "label": "Typed in"},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if added.ID != "minimaxcode:managed:"+credentialTag("mmoat_managed_0001") {
		t.Fatalf("id = %q, want a tag-derived id", added.ID)
	}

	// A discovered row is somebody else's file: removing it hides the row
	// without touching the desktop client's store.
	discovered := "minimaxcode:cn/prod/mcode-public"
	if err := c.RemoveAccount(ctx, discovered); err != nil {
		t.Fatalf("RemoveAccount(%s): %v", discovered, err)
	}
	rows, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	for _, r := range rows {
		if r.ID == discovered {
			t.Fatalf("the discovered row survived removal: %+v", rows)
		}
	}

	// Removing it twice is not an error: the operator's intent is satisfied.
	if err := c.RemoveAccount(ctx, discovered); err != nil {
		t.Fatalf("second RemoveAccount: %v", err)
	}
	// An id this module never held is still refused.
	if err := c.RemoveAccount(ctx, "minimaxcode:not-ours"); err == nil {
		t.Fatal("RemoveAccount accepted an id it does not hold")
	}

	// A managed row is really deleted, and re-adding the same credential
	// brings it back rather than silently doing nothing.
	if err := c.RemoveAccount(ctx, added.ID); err != nil {
		t.Fatalf("RemoveAccount(managed): %v", err)
	}
	again, err := c.AddAccount(ctx, core.AccountSpec{
		Fields: map[string]string{"access_token": "mmoat_managed_0001"},
	})
	if err != nil {
		t.Fatalf("re-AddAccount: %v", err)
	}
	if again.ID != added.ID {
		t.Fatalf("re-added id = %q, want %q", again.ID, added.ID)
	}
}

func TestRefreshAccountSaysWhenThereIsNothingToRenew(t *testing.T) {
	c, ft := newTestClient(t, oneAccount(), nil)
	ctx := context.Background()

	// A credential typed into the panel is an access token on its own: no
	// refresh token, no store to write back to.  That is a result, not a
	// mystery vendor error.
	results, err := c.RefreshAccount(ctx, "acct-1")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) != 1 || results[0].OK {
		t.Fatalf("results = %+v, want one honest failure", results)
	}
	if !strings.Contains(results[0].Error, "refresh token") {
		t.Fatalf("error = %q, want it to name the missing refresh token", results[0].Error)
	}
	if ft.count() != 0 {
		t.Fatalf("the module made %d upstream calls for a credential it cannot renew", ft.count())
	}

	// Refreshing everything when nothing is refreshable is a real error.
	if _, err := c.RefreshAccount(ctx, ""); err == nil {
		t.Fatal("RefreshAccount(\"\") succeeded with no refreshable account")
	}
	// So is naming an account this module never held.
	if _, err := c.RefreshAccount(ctx, "minimaxcode:nope"); err == nil {
		t.Fatal("RefreshAccount accepted an unknown id")
	}
}

func TestAccountFieldsAskForExactlyTheCredential(t *testing.T) {
	c, _ := newTestClient(t, nil, nil)

	fields := c.AccountFields(context.Background())
	var token *core.FieldSpec
	for i := range fields {
		if fields[i].Key == "access_token" {
			token = &fields[i]
		}
	}
	if token == nil {
		t.Fatalf("AccountFields = %+v, want an access_token field", fields)
	}
	if !token.Required {
		t.Fatal("access_token is not marked required")
	}
	if token.Type != "password" {
		t.Fatalf("access_token type = %q, want password", token.Type)
	}
}

// ---------------------------------------------------------------------------
// the model catalogue
// ---------------------------------------------------------------------------

const configYAMLFixture = `models:
  MiniMax-M3.1-Flash-Preview:
    name: M3.1-Flash-Preview
    reasoning: true
    attachment: true
    limit:
      context: 512000
      output: 128000
  MiniMax-M3:
    name: M3
    reasoning: true
    attachment: true
    limit:
      context: 512000
      output: 128000
  MiniMax-M2.7-highspeed:
    name: M2.7-highspeed
    reasoning: true
    limit:
      context: 200000
      output: 128000
  MiniMax-M2.7:
    name: M2.7
    reasoning: true
    limit:
      context: 200000
      output: 128000
whitelist:
  - MiniMax-M3.1-Flash-Preview
  - MiniMax-M3
  - MiniMax-M2.7-highspeed
  - MiniMax-M2.7
model_order:
  - MiniMax-M3.1-Flash-Preview
  - MiniMax-M3
  - MiniMax-M2.7-highspeed
  - MiniMax-M2.7
defaultModel: minimax/MiniMax-M3.1-Flash-Preview
`

func TestTheModelCatalogueComesFromTheDesktopClientsOwnFile(t *testing.T) {
	rows := parseModelCatalogue(configYAMLFixture)
	if len(rows) != 4 {
		t.Fatalf("parsed %d models, want 4: %+v", len(rows), rows)
	}
	if rows[0].ID != "MiniMax-M3.1-Flash-Preview" {
		t.Fatalf("first model = %q, want the whitelist's order", rows[0].ID)
	}
	if rows[0].Context != 512000 || rows[0].Output != 128000 {
		t.Fatalf("limits = %d/%d, want 512000/128000", rows[0].Context, rows[0].Output)
	}
	if !rows[0].Vision || !rows[0].Reason {
		t.Fatalf("flags = vision:%v reasoning:%v, want both true", rows[0].Vision, rows[0].Reason)
	}
	last := rows[len(rows)-1]
	if last.ID != "MiniMax-M2.7" || last.Vision {
		t.Fatalf("last model = %+v, want MiniMax-M2.7 with no attachment support", last)
	}
}

func TestAnUnreadableCatalogueFallsBackWithoutAnError(t *testing.T) {
	c, _ := newTestClient(t, map[string]any{
		"config_yaml": filepath.Join(t.TempDir(), "does-not-exist.yaml"),
	}, nil)

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != len(fallbackModels) {
		t.Fatalf("models = %+v, want the compiled-in %d", models, len(fallbackModels))
	}
	for i, m := range models {
		if m.ID != fallbackModels[i].ID {
			t.Fatalf("model[%d] = %q, want %q", i, m.ID, fallbackModels[i].ID)
		}
	}
}

func TestRefreshModelsFallsBackToTheCompiledInTableWhenTheFileGoesAway(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, configYAMLFixture)

	c, _ := newTestClient(t, map[string]any{"config_yaml": path}, nil)
	before, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(before) != 4 {
		t.Fatalf("models = %+v, want the 4 parsed from the file", before)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove fixture: %v", err)
	}
	after, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("an unreadable config.yaml must degrade to the compiled-in table, not fail: %v", err)
	}
	if len(after) != len(fallbackModels) {
		t.Fatalf("RefreshModels = %+v, want the compiled-in %d", after, len(fallbackModels))
	}
	still, _ := c.Models(context.Background())
	if len(still) != len(after) {
		t.Fatalf("Models() = %+v, want the list RefreshModels returned (%+v)", still, after)
	}
}

func TestRefreshModelsNeverReportsSuccessWithNoModels(t *testing.T) {
	// The operator's own filter excludes everything the catalogue knows about.
	// Silently answering "ok, no models" would blank the picker, so this must
	// come back as a failure.
	c, _ := newTestClient(t, map[string]any{"models": []string{"MiniMax-Not-Real"}}, nil)

	after, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatalf("RefreshModels reported success with %d models", len(after))
	}
	still, _ := c.Models(context.Background())
	if len(still) != len(after) {
		t.Fatalf("Models() = %+v, want what RefreshModels returned (%+v)", still, after)
	}
}

func TestStatusIsNotReadyWhileTheOnlyAccountIsParked(t *testing.T) {
	c, _ := newTestClient(t, oneAccount(), func(*http.Request) (*http.Response, error) {
		return jsonResponse(429, `{"message":"too many requests"}`), nil
	})

	// Before anything is tried, the row is ready.
	if status := c.Status(context.Background()); !status.Ready {
		t.Fatalf("a fresh account reported not-ready: %+v", status)
	}

	if _, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "MiniMax-M3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("Chat succeeded against a 429")
	}

	status := c.Status(context.Background())
	if status.Ready {
		t.Fatalf("Status reported ready after every account was cooled: %+v", status)
	}
	if len(status.Accounts) != 1 || status.Accounts[0].State != stateCooling {
		t.Fatalf("accounts = %+v, want one cooling row", status.Accounts)
	}
	if status.Accounts[0].Note == "" {
		t.Fatal("a cooled row carries no note explaining why")
	}
}

// ---------------------------------------------------------------------------
// token refresh is single-use
// ---------------------------------------------------------------------------

// expiredAuthFixture is the desktop store with an access token that has already
// lapsed, so every request has to renew before it can be served.
const expiredAuthFixture = `{"schemaVersion":1,"records":{` +
	`"user\u0000mcode-public":{"schemaVersion":1,"accessToken":"mmoat_stale_9001",` +
	`"refreshToken":"mmort_single_use_9001","tokenType":"Bearer","clientId":"mcode-public",` +
	`"scopes":["openid"],"audience":"agent","expiresAtMs":1000000000000,` +
	`"generation":3,"loginEpoch":"epoch-1"}}}`

const expiredAuthStateFixture = `{"schemaVersion":1,"status":"authenticated","storeKind":"file",` +
	`"clientId":"mcode-public","buildEnv":"cn","region":"prod",` +
	`"generation":3,"expiresAtMs":1000000000000}`

// The vendor retires a refresh token the moment it is exchanged.  Two requests
// that both find a lapsed token must therefore spend it exactly once: if both
// refreshed, the loser's write-back would put a retired token into the store and
// break the desktop client along with this module.
func TestConcurrentRefreshesOfOneAccountSpendTheTokenOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "auth")
	dir := filepath.Join(root, "cn", "prod", "mcode-public")
	writeFile(t, filepath.Join(dir, "auth.json"), expiredAuthFixture)
	writeFile(t, filepath.Join(dir, "auth-state.json"), expiredAuthStateFixture)

	var mu sync.Mutex
	exchanges := 0

	c, ft := newTestClient(t, map[string]any{"auto_discover": true, "auth_dir": root},
		func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/oauth2/token") {
				mu.Lock()
				exchanges++
				mu.Unlock()
				// Stay in flight long enough that the second caller is certain
				// to arrive while this exchange is still open.
				time.Sleep(150 * time.Millisecond)
				return jsonResponse(200, `{"access_token":"mmoat_fresh_9001",`+
					`"refresh_token":"mmort_fresh_9001","expires_in":3600}`), nil
			}
			return jsonResponse(200, reply), nil
		})

	// Both callers hold the same live pool entry, exactly as two concurrent
	// chats on a one-account pool would.
	first := c.pool.next(nil)
	if first == nil {
		t.Fatal("the fixture produced no account to refresh")
	}
	if !first.ExpiresAt.Before(time.Now()) {
		t.Fatalf("the fixture's token is not lapsed: expiresAt = %s", first.ExpiresAt)
	}

	results := make([]Account, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.ensureFresh(context.Background(), first.ID)
		}(i)
	}
	wg.Wait()

	mu.Lock()
	got := exchanges
	mu.Unlock()
	if got != 1 {
		t.Fatalf("the refresh token was spent %d times, want exactly 1", got)
	}

	// Every caller must end up holding the token that exchange produced.
	for i, res := range results {
		if res.Token != "mmoat_fresh_9001" {
			t.Fatalf("caller %d still carries %q, want the renewed token", i, res.Token)
		}
		if res.RefreshToken != "mmort_fresh_9001" {
			t.Fatalf("caller %d holds refresh token %q, want the rotated one", i, res.RefreshToken)
		}
		if !res.ExpiresAt.After(time.Now()) {
			t.Fatalf("caller %d got an expiry still in the past: %s", i, res.ExpiresAt)
		}
	}

	// And so must the pool row the next request will be handed.
	row, ok := c.pool.current(first.ID)
	if !ok {
		t.Fatal("the account vanished from the pool")
	}
	if row.Token != "mmoat_fresh_9001" || row.RefreshToken != "mmort_fresh_9001" {
		t.Fatalf("the pool row was left with %q / %q", row.Token, row.RefreshToken)
	}
	if !row.ExpiresAt.After(time.Now()) {
		t.Fatalf("the renewed expiry is still in the past: %s", row.ExpiresAt)
	}

	// And the store must carry the rotated token, never a retired one.
	blob, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatalf("read the store back: %v", err)
	}
	if !strings.Contains(string(blob), "mmort_fresh_9001") {
		t.Fatalf("the rotated refresh token was not written back: %s", blob)
	}
	if !strings.Contains(string(blob), "mmoat_fresh_9001") {
		t.Fatalf("the renewed access token was not written back: %s", blob)
	}

	// The only network traffic is the single exchange: no chat was involved.
	if ft.count() != 1 {
		t.Fatalf("upstream saw %d requests, want only the one exchange", ft.count())
	}
}

// TestMinimaxcodeChatNamesTheServedAccount pins the gateway-facing attribution.
// The credential that served a turn is known only inside Chat, and before this
// slot existed every success was filed under "(unrouted)".  The pool holds two
// accounts and the conversation is bound to the second one, so the value left
// on the slot can only be the account the picker actually chose -- not the
// first account in the pool, and not an empty string.
func TestMinimaxcodeChatNamesTheServedAccount(t *testing.T) {
	c, _ := newTestClient(t, twoAccounts(), func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, reply), nil
	})
	c.BindConversation("conv-served", "acct-2")

	var served string
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "MiniMax-M3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Options:  map[string]any{"conversation_id": "conv-served"},
		ServedBy: &served,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	drain(t, stream)

	if served != "acct-2" {
		t.Errorf("ServedBy = %q, want %q", served, "acct-2")
	}
}
