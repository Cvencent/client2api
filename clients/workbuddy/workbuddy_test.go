package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// --- fixtures --------------------------------------------------------------

func credJSON(access, refresh, realm, domain, uid string, expiresAt int64) string {
	return fmt.Sprintf(`{
  "auth": {
    "accessToken": %q,
    "refreshToken": %q,
    "expiresAt": %d,
    "domain": %q,
    "realm": %q
  },
  "account": {"uid": %q, "enterpriseId": "ent-1", "nickname": "Tester"}
}`, access, refresh, expiresAt, domain, realm, uid)
}

// sseFixture is a complete upstream stream: envelope-wrapped frames, a
// reasoning delta, split text, a DSML tool call inside the text, a native
// tool_calls fragment, usage, and [DONE].
const sseFixture = `data: {"code":0,"msg":"","data":{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"claude-sonnet-4","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}}

data: {"code":0,"msg":"","data":{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"reasoning_content":"thinking..."}}]}}

data: {"code":0,"msg":"","data":{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"lo "}}]}}

data: {"code":0,"msg":"","data":{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"<｜DSML｜tool_calls><｜DSML｜invoke name=\"read_file\"><｜DSML｜parameter name=\"path\">/tmp/x</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>"}}]}}

data: {"code":0,"msg":"","data":{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":"}}]}}]}}

data: {"code":0,"msg":"","data":{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"UTC\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":7}}}}

data: [DONE]
`

// --- fake transport --------------------------------------------------------

type fakeRT struct {
	mu      sync.Mutex
	calls   []*http.Request
	handler func(req *http.Request) (*http.Response, error)
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if f.handler != nil {
		return f.handler(req)
	}
	return jsonResponse(200, `{"code":0,"msg":"","data":{}}`), nil
}

func (f *fakeRT) lastCall() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func sseResponse(status int, body string, hdr http.Header) *http.Response {
	if hdr == nil {
		hdr = http.Header{}
	}
	hdr.Set("Content-Type", "text/event-stream")
	return &http.Response{
		StatusCode: status,
		Header:     hdr,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// mergeToolFragments groups streamed tool-call fragments by index, the way a
// gateway consumer does.
func mergeToolFragments(frags []core.ToolCallDelta) []core.ToolCallDelta {
	var out []core.ToolCallDelta
	seen := map[int]int{}
	for _, f := range frags {
		pos, ok := seen[f.Index]
		if !ok {
			seen[f.Index] = len(out)
			out = append(out, f)
			continue
		}
		out[pos].Arguments += f.Arguments
		if out[pos].Name == "" {
			out[pos].Name = f.Name
		}
		if out[pos].ID == "" {
			out[pos].ID = f.ID
		}
	}
	return out
}

// toolCallsOf normalises the shapes a decoded tool-call list can arrive in.
func toolCallsOf(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []map[string]any:
		out := make([]any, 0, len(t))
		for _, c := range t {
			out = append(out, c)
		}
		return out
	default:
		return nil
	}
}

// newTestClient builds a Client over a temp data dir with one credential.
func newTestClient(t *testing.T, rt http.RoundTripper) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-test.json")
	data := credJSON("access-token-abcdefgh", "refresh-token-abcdefgh", "cn",
		"copilot.tencent.com", "uid-test-0001", time.Now().Add(24*time.Hour).Unix())
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	deps := core.Deps{DataDir: dir}
	if rt != nil {
		deps.HTTPClient = &http.Client{Transport: rt}
	}
	client, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := client.(*Client)
	if !ok {
		t.Fatalf("New returned %T", client)
	}
	return c, dir
}

// --- credential parsing / persistence --------------------------------------

func TestParseAuthNestedAndFlat(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
		check   func(t *testing.T, a *Auth)
	}{
		{
			name: "nested",
			raw:  credJSON("at-1234567890", "rt-1234567890", "cn", "copilot.tencent.com", "u1", 1790000000),
			check: func(t *testing.T, a *Auth) {
				if a.AccessTokenValue() != "at-1234567890" {
					t.Fatalf("accessToken = %q", a.AccessTokenValue())
				}
				if a.RefreshTokenValue() != "rt-1234567890" {
					t.Fatalf("refreshToken = %q", a.RefreshTokenValue())
				}
				if a.UIDValue() != "u1" || a.EnterpriseIDValue() != "ent-1" {
					t.Fatalf("identity = %q/%q", a.UIDValue(), a.EnterpriseIDValue())
				}
				if a.ExpiryTime().Unix() != 1790000000 {
					t.Fatalf("expiry = %v", a.ExpiryTime())
				}
				if a.RealmName() != realmCN {
					t.Fatalf("realm = %q", a.RealmName())
				}
			},
		},
		{
			name: "flat",
			raw:  `{"accessToken":"at-flat-000000","refreshToken":"rt","domain":"www.workbuddy.ai","uid":"u2"}`,
			check: func(t *testing.T, a *Auth) {
				if a.AccessTokenValue() != "at-flat-000000" {
					t.Fatalf("accessToken = %q", a.AccessTokenValue())
				}
				if !a.IsGlobal() {
					t.Fatalf("expected global realm, got %q", a.RealmName())
				}
			},
		},
		{name: "missing access token", raw: `{"auth":{"refreshToken":"rt"}}`, wantErr: true},
		{name: "not json", raw: `nonsense`, wantErr: true},
		{name: "empty", raw: ``, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAuth([]byte(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAuth: %v", err)
			}
			if tc.check != nil {
				tc.check(t, a)
			}
		})
	}
}

func TestRealmName(t *testing.T) {
	tests := []struct {
		realm, domain, want string
	}{
		{"cn", "", realmCN},
		{"china", "", realmCN},
		{"", "copilot.tencent.com", realmCN},
		{"global", "", realmGlobal},
		{"intl", "", realmGlobal},
		{"international", "", realmGlobal},
		{"", "www.workbuddy.ai", realmGlobal},
		{"", "", realmCN},
		{"", "www.codebuddy.cn", realmCN},
	}
	for _, tc := range tests {
		a := &Auth{Realm: tc.realm, Domain: tc.domain}
		if got := a.RealmName(); got != tc.want {
			t.Errorf("realm=%q domain=%q → %q, want %q", tc.realm, tc.domain, got, tc.want)
		}
	}
}

func TestNeedsRefreshAndExpiry(t *testing.T) {
	now := time.Now()
	a := &Auth{AccessToken: "x", ExpiresAt: now.Add(time.Minute).Unix()}
	if !a.NeedsRefresh(5 * time.Minute) {
		t.Fatal("expected NeedsRefresh true within window")
	}
	if a.NeedsRefresh(10 * time.Second) {
		t.Fatal("expected NeedsRefresh false outside window")
	}
	unknown := &Auth{AccessToken: "x"}
	if unknown.NeedsRefresh(time.Hour) {
		t.Fatal("unknown expiry must not force a refresh")
	}
	if !unknown.ExpiryTime().IsZero() {
		t.Fatal("unknown expiry must be zero time")
	}
}

func TestSaveAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-a.json")
	a, err := ParseAuth([]byte(credJSON("at-roundtrip-0001", "rt-1", "global", "www.workbuddy.ai", "uid-9", 1800000000)))
	if err != nil {
		t.Fatalf("ParseAuth: %v", err)
	}
	a.FilePath = path
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		// Windows only tracks the read-only attribute, so Go always reports
		// 0666 there; the atomic writer still chmods 0600 on POSIX.
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("mode = %v, want 0600", perm)
		}
	}
	accounts, err := LoadAccounts(dir)
	if err != nil {
		t.Fatalf("LoadAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("loaded %d accounts", len(accounts))
	}
	if accounts[0].AccessTokenValue() != "at-roundtrip-0001" || accounts[0].UIDValue() != "uid-9" {
		t.Fatalf("round trip lost data: %+v", accounts[0])
	}
	if accounts[0].FilePath != path {
		t.Fatalf("FilePath = %q", accounts[0].FilePath)
	}
}

func TestSaveAtomicRefusesEmptyToken(t *testing.T) {
	a := &Auth{FilePath: filepath.Join(t.TempDir(), "x.json")}
	if err := a.SaveAtomic(); err == nil {
		t.Fatal("expected refusal for empty access token")
	}
}

func TestLoadAccountsSkipsJunkAndDedupes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("a.json", credJSON("at-aaaaaaaaaaaa", "rt", "cn", "copilot.tencent.com", "same-uid", 0))
	write("b.json", credJSON("at-bbbbbbbbbbbb", "rt", "cn", "copilot.tencent.com", "same-uid", 0))
	write("c.json", `{"unrelated":true}`)
	write("notes.txt", "not json")
	accounts, err := LoadAccounts(dir)
	if err != nil {
		t.Fatalf("LoadAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 deduped account, got %d", len(accounts))
	}
	if _, err := LoadAccounts(filepath.Join(dir, "missing")); err != nil {
		t.Fatalf("missing dir must not error: %v", err)
	}
}

// --- request body pipeline -------------------------------------------------

func TestPrepareBodyPipeline(t *testing.T) {
	src := `{
  "model": "claude-sonnet-4",
  "max_completion_tokens": 512,
  "tool_choice": {"type": "function", "function": {"name": "read_file"}},
  "messages": [
    {"role": "developer", "content": "be nice"},
    {"role": "user", "content": [{"type": "image_url", "image_url": "https://x/y.png"}]}
  ],
  "tools": [{"type": "function", "function": {"name": "read_file"}}]
}`
	out := prepareBody([]byte(src), false, nil, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["stream"] != true {
		t.Fatalf("stream = %v", obj["stream"])
	}
	if _, present := obj["max_completion_tokens"]; present {
		t.Fatal("max_completion_tokens must be removed")
	}
	if v, ok := obj["max_tokens"].(float64); !ok || int(v) != 512 {
		t.Fatalf("max_tokens = %v", obj["max_tokens"])
	}
	so, ok := obj["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("stream_options = %v", obj["stream_options"])
	}
	if tc, _ := obj["tool_choice"].(string); tc != "read_file" {
		t.Fatalf("tool_choice = %v", obj["tool_choice"])
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", obj["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("developer role not normalised: %v", first["role"])
	}
	second, _ := msgs[1].(map[string]any)
	parts, _ := second["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("parts = %v", second["content"])
	}
	part, _ := parts[0].(map[string]any)
	iu, ok := part["image_url"].(map[string]any)
	if !ok {
		t.Fatalf("image_url not objectified: %v", part["image_url"])
	}
	if iu["url"] != "https://x/y.png" {
		t.Fatalf("image url = %v", iu["url"])
	}
}

func TestPrepareBodyToolChoiceNone(t *testing.T) {
	src := `{"model":"m","tool_choice":"none","tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"hi"}]}`
	out := prepareBody([]byte(src), false, nil, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := obj["tools"]; ok {
		t.Fatal("tools must be dropped for tool_choice none")
	}
	if _, ok := obj["tool_choice"]; ok {
		t.Fatal("tool_choice must be dropped for none")
	}
}

func TestPrepareBodyKeepsExistingStreamOptions(t *testing.T) {
	src := `{"model":"m","stream_options":{"include_usage":false},"messages":[]}`
	out := prepareBody([]byte(src), false, nil, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so, _ := obj["stream_options"].(map[string]any)
	if so["include_usage"] != false {
		t.Fatalf("explicit stream_options must be preserved: %v", so)
	}
}

func TestPrepareBodyGarbage(t *testing.T) {
	if got := prepareBody([]byte("not json"), false, nil, nil); string(got) != "not json" {
		t.Fatalf("garbage body must pass through, got %q", got)
	}
	if got := prepareBody(nil, false, nil, nil); string(got) != "" {
		t.Fatalf("empty body must pass through, got %q", got)
	}
}

func TestNormalizeReasoningEffort(t *testing.T) {
	efforts := map[string][]string{"m": {"low", "high"}}
	tests := []struct {
		name, in, want string
	}{
		{"exact", `{"model":"m","reasoning_effort":"high"}`, "high"},
		{"snap down", `{"model":"m","reasoning_effort":"max"}`, "high"},
		{"snap up", `{"model":"m","reasoning_effort":"minimal"}`, "low"},
		{"camel case key", `{"model":"m","reasoningEffort":"low"}`, "low"},
		{"unknown model", `{"model":"z","reasoning_effort":"high"}`, "high"},
		{"absent", `{"model":"m"}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := prepareBody([]byte(tc.in), false, efforts, nil)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, _ := obj["reasoning_effort"].(string)
			if got != tc.want {
				t.Fatalf("reasoning_effort = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnsureConsoleSystem(t *testing.T) {
	out := ensureConsoleSystem([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected prepended system message, got %v", msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("first role = %v", first["role"])
	}
	again := ensureConsoleSystem([]byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`))
	var obj2 map[string]any
	if err := json.Unmarshal(again, &obj2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msgs2, _ := obj2["messages"].([]any); len(msgs2) != 2 {
		t.Fatalf("system message must not be duplicated: %v", msgs2)
	}
}

func TestBuildCacheKeyAndInjection(t *testing.T) {
	key := buildCacheKey("abcdefgh-1234", "conv-1")
	if !strings.HasPrefix(key, "wb2a-abcdefgh-") {
		t.Fatalf("cache key = %q", key)
	}
	if key != buildCacheKey("abcdefgh-1234", "conv-1") {
		t.Fatal("cache key must be deterministic")
	}
	if key == buildCacheKey("abcdefgh-1234", "conv-2") {
		t.Fatal("different conversations must not share a cache key")
	}
	if key == buildCacheKey("zzzzzzzz-1234", "conv-1") {
		t.Fatal("different accounts must not share a cache key")
	}

	out := InjectPromptCacheKey([]byte(`{"messages":[]}`), "abcdefgh-1234", "conv-1")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["prompt_cache_key"] != key {
		t.Fatalf("prompt_cache_key = %v", obj["prompt_cache_key"])
	}

	keep := InjectPromptCacheKey([]byte(`{"prompt_cache_key":"mine"}`), "abcdefgh-1234", "conv-1")
	var obj2 map[string]any
	if err := json.Unmarshal(keep, &obj2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj2["prompt_cache_key"] != "mine" {
		t.Fatalf("existing key must be preserved, got %v", obj2["prompt_cache_key"])
	}

	fromBody := InjectPromptCacheKey([]byte(`{"conversation_id":"body-conv"}`), "abcdefgh-1234", "arg-conv")
	var obj3 map[string]any
	if err := json.Unmarshal(fromBody, &obj3); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj3["prompt_cache_key"] != buildCacheKey("abcdefgh-1234", "body-conv") {
		t.Fatalf("body conversation id must win: %v", obj3["prompt_cache_key"])
	}
}

// --- SSE -------------------------------------------------------------------

func TestParseSSEChunk(t *testing.T) {
	tests := []struct {
		name, in string
		wantOK   bool
		wantID   string
	}{
		{"envelope", `{"code":0,"msg":"","data":{"id":"a"}}`, true, "a"},
		{"plain", `{"id":"b"}`, true, "b"},
		{"envelope with error code", `{"code":500,"msg":"boom","data":{"id":"c"}}`, true, "c"},
		{"garbage", `{oops`, false, ""},
		{"empty", ``, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obj, ok := ParseSSEChunk(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if id, _ := obj["id"].(string); id != tc.wantID {
				t.Fatalf("id = %q, want %q", id, tc.wantID)
			}
		})
	}
}

func TestReadSSEFrames(t *testing.T) {
	stream := "data: {\"id\":\"1\"}\n\n" +
		": keep-alive comment\n\n" +
		"data: {\"id\":\"2\"}\n" +
		"data: {\"id\":\"3\"}\n\n" +
		"{\"id\":\"4\"}\n\n" +
		"data: [DONE]\n\n"
	var ids []string
	done := 0
	err := ReadSSEFrames(strings.NewReader(stream), func(obj map[string]any, isDone bool) error {
		if isDone {
			done++
			return nil
		}
		id, _ := obj["id"].(string)
		ids = append(ids, id)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadSSEFrames: %v", err)
	}
	if done != 1 {
		t.Fatalf("done called %d times", done)
	}
	if strings.Join(ids, ",") != "1,2,3,4" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestReadSSEFramesEmpty(t *testing.T) {
	err := ReadSSEFrames(strings.NewReader("\n\n"), func(map[string]any, bool) error { return nil })
	if !IsEmptyStreamError(err) {
		t.Fatalf("expected empty stream error, got %v", err)
	}
}

func TestAggregate(t *testing.T) {
	msg, err := Aggregate(strings.NewReader(sseFixture))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices, _ := msg["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", msg["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	inner, _ := choice["message"].(map[string]any)
	content, _ := inner["content"].(string)
	if !strings.Contains(content, "Hello ") {
		t.Fatalf("content = %q", content)
	}
	if rc, _ := inner["reasoning_content"].(string); rc != "thinking..." {
		t.Fatalf("reasoning = %q", rc)
	}
	calls := toolCallsOf(inner["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", inner["tool_calls"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	usage, _ := msg["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("usage missing")
	}
	if n, _ := num64(usage["total_tokens"]); int(n) != 15 {
		t.Fatalf("total_tokens = %v", usage["total_tokens"])
	}
	if n, _ := num64(usage["cached_tokens"]); int(n) != 7 {
		t.Fatalf("cached_tokens alias = %v", usage["cached_tokens"])
	}
}

func TestNormalizeFrameWhitelist(t *testing.T) {
	out := normalizeFrame(map[string]any{
		"id":         "x",
		"object":     "",
		"model":      "m",
		"created":    float64(1),
		"unexpected": "drop me",
		"choices": []any{map[string]any{
			"index": float64(0),
			"delta": map[string]any{"content": "hi", "junk": "x"},
		}},
	})
	if out["object"] != "chat.completion.chunk" {
		t.Fatalf("object = %v", out["object"])
	}
	if _, ok := out["unexpected"]; ok {
		t.Fatal("unknown top-level key must be dropped")
	}
	choices, _ := out["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	if _, ok := delta["junk"]; ok {
		t.Fatal("unknown delta key must be dropped")
	}
	if choice["finish_reason"] != nil {
		t.Fatalf("finish_reason = %v, want nil", choice["finish_reason"])
	}
}

func TestNormalizeUsageCacheAliases(t *testing.T) {
	in := map[string]any{
		"prompt_tokens":           float64(100),
		"completion_tokens":       float64(10),
		"prompt_cache_hit_tokens": float64(0),
		"prompt_tokens_details":   map[string]any{"cached_tokens": float64(80)},
	}
	out := normalizeUsageCacheAliases(in)
	for _, k := range []string{"cached_tokens", "prompt_cache_hit_tokens", "cache_read_input_tokens"} {
		if n, _ := num64(out[k]); int(n) != 80 {
			t.Fatalf("%s = %v, want 80", k, out[k])
		}
	}
	details, _ := out["prompt_tokens_details"].(map[string]any)
	if n, _ := num64(details["cached_tokens"]); int(n) != 80 {
		t.Fatalf("details cached_tokens = %v", details["cached_tokens"])
	}
	if _, mutated := in["cached_tokens"]; mutated {
		t.Fatal("input map must not be mutated")
	}
}

func TestDropTruncatedToolCalls(t *testing.T) {
	if isTruncatedArguments(`{"a":1}`) {
		t.Fatal("valid arguments reported truncated")
	}
	if !isTruncatedArguments(`{"a":`) {
		t.Fatal("invalid arguments not reported truncated")
	}
	if isTruncatedArguments("") {
		t.Fatal("empty arguments are not truncated")
	}
}

// --- error classification --------------------------------------------------

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"ok", 200, `{"code":0}`, ErrNone},
		{"payment required", 402, "payment required", ErrHardCredit},
		{"credit marker", 200, "余额不足", ErrHardCredit},
		{"quota code", 429, `{"code":14018,"msg":"quota"}`, ErrHardCredit},
		{"rate limit", 429, "too many requests", ErrSoftRate},
		{"rate limit marker", 400, "rate limit exceeded", ErrSoftRate},
		{"session dead", 401, "Offline user session not found", ErrSessionDead},
		{"session dead code", 401, `{"code":12153}`, ErrSessionDead},
		{"not found", 404, "no such thing", ErrNotFound},
		{"server", 503, "upstream exploded", ErrServer},
		{"bad params", 400, "Unmarshal chat params failed", ErrBadParams},
		{"bad params code", 400, `{"code":11101,"msg":"bad"}`, ErrBadParams},
		{"content blocked", 400, "blocked by security policy", ErrContentBlocked},
		{"account fault", 400, "request illegal", ErrAccountFault},
		{"trial", 400, "trial not activated", ErrAccountFault},
		{"model blocked", 400, `{"code":11102,"msg":"service info not found"}`, ErrModelBlocked},
		{"waf", 403, "you shall not pass", ErrWafBlock},
		{"waf with envelope is client error", 403, `{"code":1,"msg":"denied"}`, ErrClient},
		{"prompt too long", 400, `{"code":11115,"msg":"prompt is too long"}`, ErrPromptTooLong},
		{"prompt too long 413", 413, "prompt is too long", ErrPromptTooLong},
		{"image invalid", 400, `{"code":11135,"msg":"invalid image_url content"}`, ErrImageInvalid},
		{"image marker", 400, "replace the image", ErrImageInvalid},
		{"generic client", 400, "something else", ErrClient},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, tc.body); got != tc.want {
				t.Fatalf("Classify(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name   string
		header string
		value  string
		want   time.Duration
		wantOK bool
	}{
		{"seconds", "Retry-After", "30", 30 * time.Second, true},
		{"millis", "Retry-After-Ms", "1500", 1500 * time.Millisecond, true},
		{"garbage", "Retry-After", "soon", 0, false},
		{"zero", "Retry-After", "0", 0, false},
		{"absurd", "Retry-After", "99999999", 0, false},
		{"past reset", "X-Ratelimit-Reset", "1", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			h.Set(tc.header, tc.value)
			got, ok := ParseRetryAfter(h)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("duration = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseRateReset(t *testing.T) {
	ts, ok := ParseRateReset("您的额度将在 2026-03-01 12:00:00 重置")
	if !ok {
		t.Fatal("CN reset not parsed")
	}
	want := time.Date(2026, 3, 1, 12, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))
	if !ts.Equal(want) {
		t.Fatalf("reset = %v, want %v", ts, want)
	}
	if _, ok := ParseRateReset("reset at 2026-03-01 12:00:00"); !ok {
		t.Fatal("EN reset not parsed")
	}
	if _, ok := ParseRateReset("nothing here"); ok {
		t.Fatal("unexpected reset parse")
	}
}

func TestClassifyErrorIsRetryableAndString(t *testing.T) {
	err := &Error{Kind: ErrHardCredit, Status: 402, Msg: "no credit"}
	if !errors.Is(err, error(err)) {
		t.Fatal("sanity")
	}
	if err.Error() == "" {
		t.Fatal("empty error string")
	}
	for _, k := range []ErrKind{ErrNone, ErrHardCredit, ErrSoftRate, ErrSessionDead, ErrNotFound,
		ErrServer, ErrContentBlocked, ErrBadParams, ErrAccountFault, ErrModelBlocked, ErrWafBlock,
		ErrPromptTooLong, ErrImageInvalid, ErrClient} {
		if k.String() == "" {
			t.Fatalf("kind %d has no string", int(k))
		}
	}
}

// --- pool ------------------------------------------------------------------

func TestCooldownFor(t *testing.T) {
	tests := []struct {
		kind      ErrKind
		retry     time.Duration
		wantState string
		wantZero  bool
	}{
		{ErrHardCredit, 0, stateExhausted, false},
		{ErrAccountFault, 0, "risk", false},
		{ErrWafBlock, 0, stateCooling, false},
		{ErrSessionDead, 0, stateCooling, false},
		{ErrSoftRate, 45 * time.Second, stateCooling, false},
		{ErrSoftRate, 0, stateCooling, false},
		{ErrServer, 0, stateCooling, false},
		{ErrNotFound, 0, stateCooling, false},
		{ErrContentBlocked, 0, stateReady, true},
		{ErrBadParams, 0, stateReady, true},
		{ErrNone, 0, stateReady, true},
	}
	for _, tc := range tests {
		d, state := cooldownFor(tc.kind, tc.retry)
		if tc.wantZero && d != 0 {
			t.Errorf("%v: duration = %v, want 0", tc.kind, d)
		}
		if !tc.wantZero && d <= 0 {
			t.Errorf("%v: duration = %v, want > 0", tc.kind, d)
		}
		if state != tc.wantState {
			t.Errorf("%v: state = %q, want %q", tc.kind, state, tc.wantState)
		}
	}
	if d, _ := cooldownFor(ErrSoftRate, 45*time.Second); d != 45*time.Second {
		t.Errorf("Retry-After must be honoured, got %v", d)
	}
}

// TestNotFoundCooldownIsFixedAndDoesNotEscalate pins the reference's 404 split
// (internal/server/handler.go: `case upstream.ErrNotFound` → a fixed
// notFoundCooldown, deliberately not the soft-rate base).  Sharing the soft-rate
// path would charge an occasionally missing path the rate-limit penalty, which
// benches a healthy account for ten minutes and doubles on the next hiccup.
func TestNotFoundCooldownIsFixedAndDoesNotEscalate(t *testing.T) {
	d, state := cooldownFor(ErrNotFound, 0)
	if d != notFoundCooldown {
		t.Errorf("404 park = %v, want the fixed %v", d, notFoundCooldown)
	}
	// Pin the magnitude the reference chose.  It happens to equal
	// softRateCooldown, so only the constant *name* distinguishes the two
	// policies -- which is exactly why the reference gave 404 its own constant
	// and why this module now does too.
	if notFoundCooldown != 60*time.Second {
		t.Errorf("notFoundCooldown = %v, want the reference's 60s", notFoundCooldown)
	}
	if state != stateCooling {
		t.Errorf("404 state = %q, want %q", state, stateCooling)
	}
	// A header is the vendor naming its own wait, so it still wins.
	if d, _ := cooldownFor(ErrNotFound, 45*time.Second); d != 45*time.Second {
		t.Errorf("404 with Retry-After = %v, want 45s", d)
	}
	// An absurd hint must not be honoured -- the same sanity bound the other
	// kinds apply.
	if d, _ := cooldownFor(ErrNotFound, 48*time.Hour); d != notFoundCooldown {
		t.Errorf("404 with an absurd Retry-After = %v, want the fixed %v", d, notFoundCooldown)
	}
	// 5xx keeps the transient fallback: the reference feeds it to the breaker
	// rather than inventing a long park, and this module adds a short one.
	if d, _ := cooldownFor(ErrServer, 0); d != shortCooldown {
		t.Errorf("5xx park = %v, want the transient %v", d, shortCooldown)
	}
}

func TestRetryableKind(t *testing.T) {
	if !retryableKind(ErrSoftRate) || !retryableKind(ErrHardCredit) {
		t.Fatal("transient/credit failures must be retryable on another account")
	}
	if retryableKind(ErrBadParams) || retryableKind(ErrContentBlocked) || retryableKind(ErrImageInvalid) {
		t.Fatal("request-shape failures must not be retried")
	}
}

func TestPoolPickRoundRobinAndCooldown(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u1"}
	b := &Auth{AccessToken: "at-222222222222", UID: "u2"}
	p := NewPool([]*Auth{a, b}, 0)
	if p.Len() != 2 {
		t.Fatalf("len = %d", p.Len())
	}
	first, ok := p.Pick(nil)
	if !ok {
		t.Fatal("expected a pick")
	}
	second, ok := p.Pick(nil)
	if !ok {
		t.Fatal("expected a second pick")
	}
	if first.ID() == second.ID() {
		t.Fatal("round robin must rotate accounts")
	}
	p.MarkFailure(a, &Error{Kind: ErrHardCredit, Status: 402, Msg: "no credit"})
	third, ok := p.Pick(nil)
	if !ok || third.ID() != b.ID() {
		t.Fatalf("cooled account must be skipped, got %v", ok)
	}
	p.MarkFailure(b, &Error{Kind: ErrHardCredit, Status: 402, Msg: "no credit"})
	if _, ok := p.Pick(nil); ok {
		t.Fatal("no account should be usable")
	}
	if p.Ready() {
		t.Fatal("pool must not be ready")
	}
	p.MarkSuccess(a)
	if !p.Ready() {
		t.Fatal("MarkSuccess must clear the cooldown")
	}
	snapshot := p.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("snapshot len = %d", len(snapshot))
	}
	for _, as := range snapshot {
		if as.ID == "" || as.State == "" {
			t.Fatalf("incomplete account status: %+v", as)
		}
	}
	if p.Summary() == "" {
		t.Fatal("empty summary")
	}
}

func TestPoolPickHonoursAccountPriority(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u1"}
	b := &Auth{AccessToken: "at-222222222222", UID: "u2"}
	c := &Auth{AccessToken: "at-333333333333", UID: "u3"}
	p := NewPool([]*Auth{a, b, c}, 0)
	core.SetAccountPriorities(map[string]map[string]int{"workbuddy": {"u2": -1}})
	t.Cleanup(func() { core.SetAccountPriorities(nil) })

	for i := 0; i < 4; i++ {
		got, ok := p.Pick(nil)
		if !ok || got.ID() != "u2" {
			t.Fatalf("pick %d = %v/%v, want the priority account u2", i, got, ok)
		}
	}
}

func TestUsableForModelRespectsAccountPriorityTier(t *testing.T) {
	low := &Auth{AccessToken: "at-111111111111", UID: "low"}
	high := &Auth{AccessToken: "at-222222222222", UID: "high"}
	p := NewPool([]*Auth{low, high}, 0)
	core.SetAccountPriorities(map[string]map[string]int{"workbuddy": {"high": -1}})
	t.Cleanup(func() { core.SetAccountPriorities(nil) })

	if p.UsableForModel("low", "model-x") {
		t.Fatal("a lower-priority account must not satisfy a sticky binding while a higher-priority account is usable")
	}
	if !p.UsableForModel("high", "model-x") {
		t.Fatal("the highest-priority usable account must be allowed as a sticky binding")
	}
}

func TestPoolSkipsRequestedAccounts(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u1"}
	b := &Auth{AccessToken: "at-222222222222", UID: "u2"}
	p := NewPool([]*Auth{a, b}, 0)
	got, ok := p.Pick(map[string]bool{a.ID(): true})
	if !ok || got.ID() != b.ID() {
		t.Fatalf("skip set ignored: ok=%v id=%v", ok, got)
	}
	if _, ok := p.Pick(map[string]bool{a.ID(): true, b.ID(): true}); ok {
		t.Fatal("all accounts skipped must return false")
	}
}

func TestPoolReplacePreservesHealth(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u1"}
	p := NewPool([]*Auth{a}, 0)
	p.MarkInvalid(a, "broken")
	again := &Auth{AccessToken: "at-111111111111", UID: "u1"}
	p.Replace([]*Auth{again})
	if p.Ready() {
		t.Fatal("health state must survive a reload")
	}
}

// --- DSML ------------------------------------------------------------------

func TestDSMLParserBlocks(t *testing.T) {
	full := "<｜DSML｜tool_calls>\n<｜DSML｜invoke name=\"read_file\">\n" +
		"<｜DSML｜parameter name=\"path\">/tmp/x</｜DSML｜parameter>\n" +
		"<｜DSML｜parameter name=\"encoding\">utf-8</｜DSML｜parameter>\n" +
		"</｜DSML｜invoke>\n</｜DSML｜tool_calls>"
	tests := []struct {
		name      string
		chunks    []string
		wantText  string
		wantCalls int
		wantName  string
	}{
		{name: "plain text", chunks: []string{"hello world"}, wantText: "hello world"},
		{name: "single block", chunks: []string{full}, wantText: "", wantCalls: 1, wantName: "read_file"},
		{name: "text around block", chunks: []string{"before ", full, " after"}, wantText: "before  after", wantCalls: 1, wantName: "read_file"},
		{name: "xml without prefix", chunks: []string{`<tool_calls><invoke name="f"><parameter name="a">1</parameter></invoke></tool_calls>`}, wantCalls: 1, wantName: "f"},
		{name: "antml prefix", chunks: []string{`<antml:tool_calls><antml:invoke name="g"><antml:parameter name="a">2</antml:parameter></antml:invoke></antml:tool_calls>`}, wantCalls: 1, wantName: "g"},
		{name: "bare invoke", chunks: []string{`<invoke name="h"><parameter name="a">3</parameter></invoke>`}, wantCalls: 1, wantName: "h"},
		{name: "json payload", chunks: []string{`<tool_calls>[{"name":"j","arguments":{"a":1}}]</tool_calls>`}, wantCalls: 1, wantName: "j"},
		{name: "no tool call", chunks: []string{"just <b>markup</b> text"}, wantText: "just <b>markup</b> text"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewDSMLParser()
			var text strings.Builder
			var calls []dsmlCall
			for _, chunk := range tc.chunks {
				out, cs := p.Feed(chunk)
				text.WriteString(out)
				calls = append(calls, cs...)
			}
			out, cs := p.Finish()
			text.WriteString(out)
			calls = append(calls, cs...)
			if got := text.String(); got != tc.wantText {
				t.Fatalf("text = %q, want %q", got, tc.wantText)
			}
			if len(calls) != tc.wantCalls {
				t.Fatalf("calls = %d (%v), want %d", len(calls), calls, tc.wantCalls)
			}
			if tc.wantCalls > 0 {
				if calls[0].Name != tc.wantName {
					t.Fatalf("call name = %q, want %q", calls[0].Name, tc.wantName)
				}
				var args map[string]any
				if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
					t.Fatalf("arguments %q are not JSON: %v", calls[0].Arguments, err)
				}
			}
		})
	}
}

func TestDSMLParserStreamedByteByByte(t *testing.T) {
	full := "<｜DSML｜tool_calls><｜DSML｜invoke name=\"read_file\"><｜DSML｜parameter name=\"path\">/tmp/x</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>"
	p := NewDSMLParser()
	var text strings.Builder
	var calls []dsmlCall
	for _, r := range full {
		out, cs := p.Feed(string(r))
		text.WriteString(out)
		calls = append(calls, cs...)
	}
	out, cs := p.Finish()
	text.WriteString(out)
	calls = append(calls, cs...)
	if text.String() != "" {
		t.Fatalf("text = %q, want empty", text.String())
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %v", calls)
	}
	if calls[0].Name != "read_file" {
		t.Fatalf("name = %q", calls[0].Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments: %v", err)
	}
	if args["path"] != "/tmp/x" {
		t.Fatalf("args = %v", args)
	}
}

func TestDSMLParserFinishFlushesUnterminatedBlock(t *testing.T) {
	// A complete <invoke> inside a wrapper that never closes: the call is
	// still recovered when the stream ends.
	p := NewDSMLParser()
	partial := `<｜DSML｜tool_calls><｜DSML｜invoke name="f"><｜DSML｜parameter name="a">1</｜DSML｜parameter></｜DSML｜invoke>`
	if out, calls := p.Feed(partial); out != "" || len(calls) != 0 {
		t.Fatalf("expected buffering, got %q / %v", out, calls)
	}
	out, calls := p.Finish()
	if out != "" {
		t.Fatalf("out = %q", out)
	}
	if len(calls) != 1 || calls[0].Name != "f" {
		t.Fatalf("calls = %v", calls)
	}

	// A truncated <invoke> with no closing tag is dropped rather than emitted
	// with half-parsed arguments.
	truncated := NewDSMLParser()
	truncated.Feed(`<｜DSML｜tool_calls><｜DSML｜invoke name="f">`)
	if _, calls := truncated.Finish(); len(calls) != 0 {
		t.Fatalf("truncated call must be dropped, got %v", calls)
	}
}

func TestHasDSMLMarker(t *testing.T) {
	if !HasDSMLMarker("x <｜DSML｜tool_calls> y") || !HasDSMLMarker("antml:invoke") {
		t.Fatal("markers not detected")
	}
	if HasDSMLMarker("plain text") {
		t.Fatal("false positive")
	}
}

// --- helpers ---------------------------------------------------------------

func TestNormalizeFinish(t *testing.T) {
	tests := []struct {
		in    string
		tools bool
		want  string
	}{
		{"stop", false, "stop"},
		{"stop", true, "tool_calls"},
		{"", false, "stop"},
		{"", true, "tool_calls"},
		{"length", true, "length"},
		{"content_filter", false, "content_filter"},
		{"function_call", false, "tool_calls"},
		{"tool_calls", false, "tool_calls"},
		{"weird", false, "stop"},
	}
	for _, tc := range tests {
		if got := normalizeFinish(tc.in, tc.tools); got != tc.want {
			t.Errorf("normalizeFinish(%q, %v) = %q, want %q", tc.in, tc.tools, got, tc.want)
		}
	}
}

func TestUsageFromMap(t *testing.T) {
	if u := usageFromMap(map[string]any{}); u != nil {
		t.Fatalf("empty usage must be nil, got %+v", u)
	}
	u := usageFromMap(map[string]any{
		"prompt_tokens":           float64(10),
		"completion_tokens":       float64(5),
		"prompt_cache_hit_tokens": float64(4),
	})
	if u == nil {
		t.Fatal("usage missing")
	}
	if u.TotalTokens != 15 || u.CachedTokens != 4 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestNewMessageIDIsHex32(t *testing.T) {
	id := newMessageID()
	if len(id) != 32 {
		t.Fatalf("message id = %q", id)
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("message id %q is not hex", id)
		}
	}
	if newMessageID() == id {
		t.Fatal("message ids must differ")
	}
}

func TestDeriveAccountStableID(t *testing.T) {
	got := deriveAccountStableID("uid-1", "machine")
	if len(got) != 36 {
		t.Fatalf("stable id = %q (len %d)", got, len(got))
	}
	if got != deriveAccountStableID("uid-1", "machine") {
		t.Fatal("stable id must be deterministic")
	}
	if got == deriveAccountStableID("uid-2", "machine") {
		t.Fatal("stable id must depend on the uid")
	}
	if got == deriveAccountStableID("uid-1", "session") {
		t.Fatal("stable id must depend on the purpose")
	}
}

func TestNonChatModel(t *testing.T) {
	if !nonChatModel("nes-turbo", 0, nil) || !nonChatModel("codewise-x", 0, nil) {
		t.Fatal("non-chat prefixes not filtered")
	}
	if !nonChatModel("tiny", 128, nil) {
		t.Fatal("small output cap not filtered")
	}
	if !nonChatModel("image", 4096, []string{"text-to-image"}) {
		t.Fatal("text-to-image tag not filtered")
	}
	if nonChatModel("claude-sonnet-4", 8192, []string{"chat"}) {
		t.Fatal("chat model wrongly filtered")
	}
}

func TestMergeModelInfos(t *testing.T) {
	primary := []ModelInfo{{ID: "a"}, {ID: "b"}}
	secondary := []ModelInfo{{ID: "b"}, {ID: "c"}}
	merged := mergeModelInfos(primary, secondary)
	if len(merged) != 3 {
		t.Fatalf("merged = %v", merged)
	}
	if merged[0].ID != "a" || merged[1].ID != "b" || merged[2].ID != "c" {
		t.Fatalf("order = %v", merged)
	}
}

// --- client surface --------------------------------------------------------

func TestNameAndRegistration(t *testing.T) {
	c, _ := newTestClient(t, nil)
	if c.Name() != "workbuddy" {
		t.Fatalf("Name = %q", c.Name())
	}
	found := false
	for _, n := range core.Registered() {
		if n == "workbuddy" {
			found = true
		}
	}
	if !found {
		t.Fatal("workbuddy not registered")
	}
}

func TestNewWithBadConfigUsesDefaults(t *testing.T) {
	dir := t.TempDir()
	client, err := New(core.Deps{DataDir: dir, Config: []byte("{not json")})
	if err != nil {
		t.Fatalf("New must tolerate bad config: %v", err)
	}
	if client.Name() != "workbuddy" {
		t.Fatalf("Name = %q", client.Name())
	}
}

func TestStatusWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	client, err := New(core.Deps{DataDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	st := client.Status(context.Background())
	if st.Ready {
		t.Fatal("Ready must be false without credentials")
	}
	if st.Name != "workbuddy" {
		t.Fatalf("Name = %q", st.Name)
	}
	if !strings.Contains(st.Detail, dir) {
		t.Fatalf("Detail should name the data dir: %q", st.Detail)
	}
	// The catalogue is pure-dynamic: with no credential there is nothing to
	// discover, so Status must report an empty list rather than a guess.
	if len(st.Models) != 0 {
		t.Fatalf("Status must not advertise undiscovered models, got %v", st.Models)
	}
	if st.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt must be set")
	}
}

func TestStatusWithCredentials(t *testing.T) {
	c, _ := newTestClient(t, nil)
	st := c.Status(context.Background())
	if !st.Ready {
		t.Fatalf("Ready = false, detail = %q", st.Detail)
	}
	if len(st.Accounts) != 1 {
		t.Fatalf("accounts = %+v", st.Accounts)
	}
	acc := st.Accounts[0]
	if acc.State != stateReady || !acc.Enabled {
		t.Fatalf("account = %+v", acc)
	}
	if acc.Note != "" {
		t.Fatalf("unexpected note %q", acc.Note)
	}
	if acc.Extra["realm"] != realmCN {
		t.Fatalf("realm extra = %v", acc.Extra)
	}
}

// The reference is pure-dynamic: with no account and no configured list there is
// nothing to serve, and the module must say so instead of inventing ids the
// backend may no longer accept (those answer 11102).
func TestModelsArePureDynamicOffline(t *testing.T) {
	dir := t.TempDir()
	client, err := New(core.Deps{DataDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	models, err := client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("offline Models must not fabricate a catalogue, got %+v", models)
	}
	if st := client.Status(context.Background()); len(st.Models) != 0 {
		t.Fatalf("Status must report the same empty catalogue, got %v", st.Models)
	}
}

func TestModelsUsesConfiguredFallback(t *testing.T) {
	dir := t.TempDir()
	client, err := New(core.Deps{DataDir: dir, Config: []byte(`{"models":["only-this"]}`)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	models, err := client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 || models[0].ID != "only-this" {
		t.Fatalf("models = %+v", models)
	}
}

// A failed catalogue fetch arms the negative cache: a client that polls
// /v1/models through an outage must not turn that outage into a flood.  An
// explicit refresh is an operator demand and still probes; its success clears
// the cache again.
func TestModelsFailureArmsTheNegativeCacheAndSuccessClearsIt(t *testing.T) {
	failing := true
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if failing {
			return jsonResponse(http.StatusServiceUnavailable, `{"code":1,"msg":"upstream is down"}`), nil
		}
		if strings.HasSuffix(req.URL.Path, enterpriseModelsPth) {
			return jsonResponse(http.StatusOK, catalogueBody("glm-5")), nil
		}
		return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"probe disabled"}`), nil
	}}
	c, _ := newTestClient(t, rt)

	if models, err := c.Models(context.Background()); err != nil || len(models) != 0 {
		t.Fatalf("first Models = %#v, %v; want an empty catalogue and no error", models, err)
	}
	first := totalCalls(rt)
	if first == 0 {
		t.Fatal("the first Models must have probed upstream")
	}
	if models, err := c.Models(context.Background()); err != nil || len(models) != 0 {
		t.Fatalf("second Models = %#v, %v", models, err)
	}
	if got := totalCalls(rt); got != first {
		t.Fatalf("the negative cache must suppress the second probe: %d -> %d calls", first, got)
	}

	failing = false
	// The catalogue is realm-qualified: this fixture's only account is a CN one,
	// so the id it publishes carries the cn prefix.
	models, err := c.RefreshModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "cn:glm-5" {
		t.Fatalf("RefreshModels = %#v, %v", models, err)
	}
	// The success cleared the negative cache.  Age the catalogue artificially so
	// the next Models() has to decide whether it may probe.
	c.modelsMu.Lock()
	c.modelsAt = time.Time{}
	c.modelsMu.Unlock()
	before := totalCalls(rt)
	got, err := c.Models(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "cn:glm-5" {
		t.Fatalf("Models after a success = %#v, %v", got, err)
	}
	if after := totalCalls(rt); after <= before {
		t.Fatalf("a cleared negative cache must let Models probe again: %d -> %d calls", before, after)
	}
}

// totalCalls reports how many upstream calls the fake transport saw.
func totalCalls(rt *fakeRT) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return len(rt.calls)
}

// countPath reports how many recorded upstream calls hit a path suffix.
func countPath(rt *fakeRT, suffix string) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	n := 0
	for _, req := range rt.calls {
		if strings.HasSuffix(req.URL.Path, suffix) {
			n++
		}
	}
	return n
}

// catalogueBody renders the enterprise probe's response for one model id.
func catalogueBody(id string) string {
	return `{"code":0,"data":{"models":[{"id":"` + id + `","modelId":"` + id +
		`","maxOutputTokens":8192,"tags":[]}],"agents":[{"name":"cli","models":["` + id + `"]}]}}`
}

// ---- RefreshModels (the panel's "re-fetch from upstream" button) ----------

func TestRefreshModelsUpdatesCatalogueAndBypassesCache(t *testing.T) {
	body := catalogueBody("claude-sonnet-4")
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, enterpriseModelsPth) {
			return jsonResponse(http.StatusOK, body), nil
		}
		return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"probe disabled"}`), nil
	}}
	c, _ := newTestClient(t, rt)

	first, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(first) != 1 || first[0].ID != "cn:claude-sonnet-4" {
		t.Fatalf("Models = %#v", first)
	}
	// A second Models() inside modelsTTL must not touch the network.
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("second Models: %v", err)
	}
	if n := countPath(rt, enterpriseModelsPth); n != 1 {
		t.Fatalf("Models should be cached, saw %d catalogue fetches", n)
	}

	// The vendor changes its catalogue.  RefreshModels has to see the new
	// list even though the cache is still warm.
	body = catalogueBody("glm-5")
	refreshed, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if n := countPath(rt, enterpriseModelsPth); n != 2 {
		t.Fatalf("RefreshModels must bypass the cache, saw %d catalogue fetches", n)
	}
	if len(refreshed) != 1 || refreshed[0].ID != "cn:glm-5" {
		t.Fatalf("refreshed = %#v", refreshed)
	}

	// The refreshed list is what Models() now serves, and it is cached again.
	again, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models after refresh: %v", err)
	}
	if len(again) != 1 || again[0].ID != "cn:glm-5" {
		t.Fatalf("Models after refresh = %#v", again)
	}
	if n := countPath(rt, enterpriseModelsPth); n != 2 {
		t.Errorf("the refreshed list should be cached, saw %d catalogue fetches", n)
	}
}

func TestRefreshModelsFailureKeepsLastGoodList(t *testing.T) {
	failing := false
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if failing {
			return jsonResponse(http.StatusServiceUnavailable, `{"code":1,"msg":"upstream is down"}`), nil
		}
		if strings.HasSuffix(req.URL.Path, enterpriseModelsPth) {
			return jsonResponse(http.StatusOK, enterpriseModelsFixture), nil
		}
		return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"probe disabled"}`), nil
	}}
	c, _ := newTestClient(t, rt)

	good, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(good) != 2 {
		t.Fatalf("good = %#v", good)
	}

	failing = true
	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("a failed refresh must report an error")
	}
	// A failed refresh must never empty the catalogue.
	if len(models) != len(good) || models[0].ID != good[0].ID {
		t.Fatalf("a failed refresh must keep the last good list, got %#v", models)
	}
}

func TestRefreshModelsErrorOmitsTokens(t *testing.T) {
	// A bare token has no label for core.Redact to key on, so this also pins
	// the explicit replacement in scrubSecret.
	const token = "access-token-abcdefgh"
	rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"code":1,"msg":"credential `+token+` rejected"}`), nil
	}}
	c, _ := newTestClient(t, rt)

	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if len(models) != 0 {
		t.Fatalf("a failed refresh with nothing cached must not fabricate a catalogue, got %+v", models)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the token leaked into the error: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "workbuddy:") {
		t.Errorf("the error should be prefixed with the module name: %v", err)
	}
}

func TestChatWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	client, err := New(core.Deps{DataDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Chat(context.Background(), &core.ChatRequest{
		Model:    "claude-sonnet-4",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestChatRejectsBadRequests(t *testing.T) {
	c, _ := newTestClient(t, nil)
	if _, err := c.Chat(context.Background(), nil); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("nil request: %v", err)
	}
	if _, err := c.Chat(context.Background(), &core.ChatRequest{}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("empty model: %v", err)
	}
}

func TestChatEndToEndStreamsEvents(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/v2/chat/completions") {
			t.Errorf("unexpected path %q", req.URL.Path)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer access-token-abcdefgh" {
			t.Errorf("Authorization = %q", got)
		}
		return sseResponse(200, sseFixture, nil), nil
	}}
	c, _ := newTestClient(t, rt)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "claude-sonnet-4",
		User:     "conv-1",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	var (
		text      strings.Builder
		reasoning strings.Builder
		calls     []core.ToolCallDelta
		usage     *core.Usage
		usageSeen int
		done      int
		finish    string
	)
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch ev.Type {
		case core.EventDelta:
			text.WriteString(ev.Delta)
			reasoning.WriteString(ev.Reasoning)
		case core.EventToolCall:
			if ev.ToolCall == nil {
				t.Fatal("tool call event without payload")
			}
			calls = append(calls, *ev.ToolCall)
		case core.EventUsage:
			usageSeen++
			usage = ev.Usage
		case core.EventDone:
			done++
			finish = ev.Finish
		case core.EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}

	if text.String() != "Hello " {
		t.Fatalf("text = %q", text.String())
	}
	if reasoning.String() != "thinking..." {
		t.Fatalf("reasoning = %q", reasoning.String())
	}
	merged := mergeToolFragments(calls)
	if len(merged) != 2 {
		t.Fatalf("tool calls = %+v (raw %+v)", merged, calls)
	}
	if merged[0].Name != "read_file" || merged[0].Arguments != `{"path":"/tmp/x"}` {
		t.Fatalf("DSML call = %+v", merged[0])
	}
	if merged[1].Name != "get_time" || merged[1].Arguments != `{"tz":"UTC"}` {
		t.Fatalf("native call = %+v", merged[1])
	}
	if merged[0].Index == merged[1].Index {
		t.Fatalf("tool call indexes must differ: %+v", merged)
	}
	if usageSeen != 1 {
		t.Fatalf("usage events = %d", usageSeen)
	}
	if usage == nil || usage.TotalTokens != 15 || usage.CachedTokens != 7 {
		t.Fatalf("usage = %+v", usage)
	}
	if done != 1 {
		t.Fatalf("done events = %d", done)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %q", finish)
	}

	// A second Recv after the stream ended must keep returning io.EOF.
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("post-EOF Recv = %v", err)
	}
	// Close must be idempotent.
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestChatRequestBodyIsPrepared(t *testing.T) {
	var captured []byte
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		buf, _ := io.ReadAll(req.Body)
		captured = buf
		return sseResponse(200, "data: [DONE]\n\n", nil), nil
	}}
	c, _ := newTestClient(t, rt)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "claude-sonnet-4",
		User:     "conv-42",
		Messages: []core.Message{{Role: "developer", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	var obj map[string]any
	if err := json.Unmarshal(captured, &obj); err != nil {
		t.Fatalf("captured body is not JSON: %v (%s)", err, captured)
	}
	if obj["stream"] != true {
		t.Fatalf("stream = %v", obj["stream"])
	}
	if key, _ := obj["prompt_cache_key"].(string); !strings.HasPrefix(key, "wb2a-") {
		t.Fatalf("prompt_cache_key = %v", obj["prompt_cache_key"])
	}
	msgs, _ := obj["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("developer role not normalised: %v", first)
	}
	if obj["model"] != "claude-sonnet-4" {
		t.Fatalf("model = %v", obj["model"])
	}
}

func TestChatClassifiesUpstreamFailure(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		resp := jsonResponse(429, `{"code":0,"msg":"too many requests"}`)
		resp.Header.Set("Retry-After", "30")
		return resp, nil
	}}
	c, _ := newTestClient(t, rt)
	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "claude-sonnet-4",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("err = %T %v", err, err)
	}
	if ue.Kind != ErrSoftRate {
		t.Fatalf("kind = %v", ue.Kind)
	}
	if ue.RetryAfter != 30*time.Second {
		t.Fatalf("retry after = %v", ue.RetryAfter)
	}
	// The account must now be cooling.
	st := c.Status(context.Background())
	if st.Ready {
		t.Fatalf("account should be cooling, status = %+v", st)
	}
	if len(st.Accounts) != 1 || st.Accounts[0].State != stateCooling {
		t.Fatalf("accounts = %+v", st.Accounts)
	}
}

func TestChatRefreshesExpiredToken(t *testing.T) {
	refreshed := 0
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "token/refresh"):
			refreshed++
			return jsonResponse(200, `{"code":0,"msg":"","data":{"accessToken":"new-access-token","refreshToken":"new-refresh-token","expiresIn":3600,"domain":"copilot.tencent.com"}}`), nil
		case strings.HasSuffix(req.URL.Path, "/v2/chat/completions"):
			return sseResponse(200, "data: [DONE]\n\n", nil), nil
		default:
			return jsonResponse(404, `{"code":1,"msg":"nope"}`), nil
		}
	}}
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-expired.json")
	// Expired 10 minutes ago: inside the default 5 minute refresh window.
	data := credJSON("old-access-token", "old-refresh-token", "cn", "copilot.tencent.com",
		"uid-expired", time.Now().Add(-10*time.Minute).Unix())
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	client, err := New(core.Deps{DataDir: dir, HTTPClient: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := client.Chat(context.Background(), &core.ChatRequest{
		Model:    "claude-sonnet-4",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	if refreshed != 1 {
		t.Fatalf("refresh calls = %d", refreshed)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var doc authDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Auth.AccessToken != "new-access-token" {
		t.Fatalf("refreshed token not persisted: %q", doc.Auth.AccessToken)
	}
}

func TestChatStreamErrorEvent(t *testing.T) {
	body := "data: {\"error\":{\"message\":\"model overloaded\",\"type\":\"upstream_error\"}}\n\ndata: [DONE]\n\n"
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return sseResponse(200, body, nil), nil
	}}
	c, _ := newTestClient(t, rt)
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "claude-sonnet-4",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	var sawError bool
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Type == core.EventError {
			sawError = true
			if ev.Err == nil || !strings.Contains(ev.Err.Error(), "model overloaded") {
				t.Fatalf("error event = %v", ev.Err)
			}
		}
	}
	if !sawError {
		t.Fatal("expected an error event")
	}
}

func TestChatStreamCancellation(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return sseResponse(200, sseFixture, nil), nil
	}}
	c, _ := newTestClient(t, rt)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := c.Chat(ctx, &core.ChatRequest{
		Model:    "claude-sonnet-4",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	cancel()
	// Draining must terminate promptly rather than hang.
	deadline := time.After(5 * time.Second)
	for {
		done := make(chan struct{})
		go func() {
			stream.Recv()
			close(done)
		}()
		select {
		case <-done:
		case <-deadline:
			t.Fatal("Recv did not return after cancellation")
		}
		break
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestChatStreamEmptyUpstreamBody(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return sseResponse(200, "\n\n", nil), nil
	}}
	c, _ := newTestClient(t, rt)
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "claude-sonnet-4",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()
	var sawError bool
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Type == core.EventError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("an empty upstream body must surface as an error event")
	}
}

func TestVendorStoreNoteNeverPanics(t *testing.T) {
	_ = VendorStoreNote()
}

func TestExtractClientIP(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	// Only proxy-supplied headers are reported: echoing RemoteAddr would just
	// name our own proxy, never the real caller.
	if got := ExtractClientIP(req); got != "" {
		t.Fatalf("ip = %q, want empty", got)
	}
	req.Header.Set("X-Forwarded-For", "198.51.100.7, 192.0.2.1")
	if got := ExtractClientIP(req); got != "198.51.100.7" {
		t.Fatalf("forwarded ip = %q", got)
	}
}

func TestChatHeadersRealmSplit(t *testing.T) {
	u := NewUpstream()
	cn := &Auth{AccessToken: "at-cn-000000000000", UID: "u1", EnterpriseID: "e1", Realm: "cn", Domain: "copilot.tencent.com"}
	global := &Auth{AccessToken: "at-gl-000000000000", UID: "u2", Realm: "global", Domain: "www.workbuddy.ai"}

	reqCN, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	u.ChatHeaders(reqCN, cn, "", ChatMeta{ConversationID: "c1"})
	if got := reqCN.Header.Get("X-Enterprise-Id"); got != "e1" {
		t.Fatalf("cn enterprise header = %q", got)
	}
	if got := reqCN.Header.Get("Authorization"); got != "Bearer at-cn-000000000000" {
		t.Fatalf("cn auth header = %q", got)
	}
	if reqCN.Header.Get("X-Refresh-Token") != "" {
		t.Fatal("chat must never send the refresh token")
	}

	reqGlobal, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	u.ChatHeaders(reqGlobal, global, "", ChatMeta{ConversationID: "c2"})
	if got := reqGlobal.Header.Get("X-No-Enterprise-Id"); got != "1" {
		t.Fatalf("global enterprise suppression = %q", got)
	}
	if got := reqGlobal.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Fatalf("global domain header = %q", got)
	}
	if got := reqGlobal.Header.Get("X-Conversation-ID"); got != "c2" {
		t.Fatalf("conversation header = %q", got)
	}
}

func TestChatBaseRealmRouting(t *testing.T) {
	u := NewUpstream()
	cn := &Auth{Realm: "cn", Domain: "copilot.tencent.com"}
	global := &Auth{Realm: "global", Domain: "www.workbuddy.ai"}
	if got := u.chatBase(cn); got != "https://copilot.tencent.com" {
		t.Fatalf("cn base = %q", got)
	}
	if got := u.chatBase(global); got != "https://www.workbuddy.ai" {
		t.Fatalf("global base = %q", got)
	}
	if got := u.chatPaths(cn); len(got) != 1 || got[0] != "/v2/chat/completions" {
		t.Fatalf("chat paths = %v", got)
	}
	if paths := u.billingMeterPaths(global); len(paths) != 2 {
		t.Fatalf("global billing paths = %v", paths)
	}
	if paths := u.billingMeterPaths(cn); len(paths) != 1 || paths[0] != "/v2/billing/meter/get-user-resource" {
		t.Fatalf("cn billing paths = %v", paths)
	}
	u.GlobalEnabled = false
	if got := u.chatBase(global); got != "https://copilot.tencent.com" {
		t.Fatalf("global disabled base = %q", got)
	}
	if paths := u.billingMeterPaths(global); len(paths) != 1 {
		t.Fatalf("global disabled billing paths = %v", paths)
	}
}

func TestGlobalBillingBaseIsOverridableApartFromTheChatBase(t *testing.T) {
	u := NewUpstream()
	global := &Auth{Realm: "global", Domain: "www.workbuddy.ai"}

	// Out of the box the reference's two global hosts coincide, and the register
	// wizard rides the billing one (internal/upstream/global_register.go:44).
	if got := u.globalBillingBase(); got != defaultGlobalBase {
		t.Fatalf("default global billing base = %q, want %q", got, defaultGlobalBase)
	}
	if got := u.billingBase(global); got != defaultGlobalBase {
		t.Fatalf("default global billingBase() = %q", got)
	}

	// The reference keeps `global.chat_base` and `global.billing_base` as
	// separate knobs (internal/upstream/client.go:663-664), so moving one must
	// not move the other.
	u.ChatBaseGlobal = "https://chat.example"
	u.BillingBaseGlobal = "https://billing.example"
	if got := u.chatBase(global); got != "https://chat.example" {
		t.Fatalf("global chat base = %q", got)
	}
	if got := u.billingBase(global); got != "https://billing.example" {
		t.Fatalf("global billing base = %q", got)
	}
	// The reference hard-codes the international site for the reward-claim
	// endpoints (internal/upstream/client.go:836-844), so moving the billing
	// host must NOT move webBase.
	if got := u.webBase(global); got != defaultGlobalBase {
		t.Fatalf("global web base = %q, want %q", got, defaultGlobalBase)
	}

	// The cn realm is untouched by either override.
	cn := &Auth{Realm: "cn", Domain: "copilot.tencent.com"}
	if got := u.billingBase(cn); got != "https://www.codebuddy.cn" {
		t.Fatalf("cn billing base = %q", got)
	}
	if got := u.webBase(cn); got != "https://www.workbuddy.cn" {
		t.Fatalf("cn web base = %q", got)
	}

	// An empty override falls back to the built-in default, which is the
	// reference's globalBillingBase() rule.
	u.BillingBaseGlobal = ""
	if got := u.billingBase(global); got != defaultGlobalBase {
		t.Fatalf("empty override billing base = %q", got)
	}
}

func TestWorkbuddyConfigCarriesTheGlobalBillingBase(t *testing.T) {
	client, err := New(core.Deps{
		DataDir: t.TempDir(),
		Config:  json.RawMessage(`{"billing_base_global":"https://billing.example","chat_base_global":"https://chat.example"}`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := client.(*Client)
	if !ok {
		t.Fatalf("New returned %T", client)
	}
	if c.up == nil {
		t.Fatal("no upstream")
	}
	if got := c.up.BillingBaseGlobal; got != "https://billing.example" {
		t.Fatalf("BillingBaseGlobal = %q, want the configured value", got)
	}
	if got := c.up.ChatBaseGlobal; got != "https://chat.example" {
		t.Fatalf("ChatBaseGlobal = %q, want the configured value", got)
	}
	if got := c.globalRegisterBase(); got != "https://billing.example" {
		t.Fatalf("globalRegisterBase() = %q, want the billing host", got)
	}
}

func TestRefreshTokenRequiresRefreshToken(t *testing.T) {
	u := NewUpstream()
	a := &Auth{AccessToken: "at-000000000000", Realm: "cn", Domain: "copilot.tencent.com"}
	if err := u.RefreshToken(a); err == nil {
		t.Fatal("expected an error without a refresh token")
	}
}

func TestDoJSONClassifiesEnvelopeError(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"code":11101,"msg":"Unmarshal chat params failed"}`), nil
	}}
	u := NewUpstream()
	u.HTTP = &http.Client{Transport: rt}
	req, _ := http.NewRequest(http.MethodGet, "https://copilot.tencent.com/v3/config", nil)
	_, err := u.doJSON(req)
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v", err)
	}
	if ue.Kind != ErrBadParams {
		t.Fatalf("kind = %v", ue.Kind)
	}
}

func TestMonitorBodyIdleTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	body := monitorBody(pr, 100*time.Millisecond, func() {})
	buf := make([]byte, 4)
	if _, err := body.Read(buf); err == nil {
		t.Fatal("expected an idle timeout error")
	}
	if err := body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestParseAuthRejectsUnrelatedJSON(t *testing.T) {
	if _, err := ParseAuth([]byte(`{"unrelated":1}`)); err == nil {
		t.Fatal("expected an error for a document without an access token")
	}
}

func TestBodyBuilderRejectsNil(t *testing.T) {
	if _, err := buildWireBody(nil); err == nil {
		t.Fatal("expected an error for a nil request")
	}
	body, err := buildWireBody(&core.ChatRequest{
		Model: "m",
		Messages: []core.Message{{
			Role:    "user",
			Content: "look",
			Parts: []core.ContentPart{
				{Type: "text", Text: "look"},
				{Type: "image_url", ImageURL: "https://x/y.png"},
			},
		}},
		Tools: []core.Tool{{Type: "function", Name: "f", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("buildWireBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := obj["tools"].([]any); !ok {
		t.Fatalf("tools missing: %v", obj["tools"])
	}
}

func TestConfigHelpers(t *testing.T) {
	zero := 0
	three := 3
	cfg := config{MaxAttempts: &three, RefreshWindowSecs: &zero}
	if cfg.maxAttempts() != 3 {
		t.Fatalf("maxAttempts = %d", cfg.maxAttempts())
	}
	if cfg.refreshWindow() != 0 {
		t.Fatalf("refreshWindow = %v", cfg.refreshWindow())
	}
	def := config{}
	if def.maxAttempts() != defaultMaxAttempts || def.refreshWindow() != defaultRefreshWindow {
		t.Fatalf("defaults = %d/%v", def.maxAttempts(), def.refreshWindow())
	}
}

func TestOptionString(t *testing.T) {
	opts := map[string]any{"conversationId": "c9", "n": 1}
	if got := optionString(opts, "conversation_id", "conversationId"); got != "c9" {
		t.Fatalf("got %q", got)
	}
	if got := optionString(opts, "missing"); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := optionString(nil, "x"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestMaskedSecretsNeverLeak(t *testing.T) {
	const token = "super-secret-access-token-value"
	if strings.Contains(core.MaskSecret(token), token) {
		t.Fatal("MaskSecret must not return the raw secret")
	}
	dir := t.TempDir()
	client, err := New(core.Deps{DataDir: dir, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	st := client.Status(context.Background())
	blob, _ := json.Marshal(st)
	if bytes.Contains(blob, []byte(token)) {
		t.Fatal("status must never contain a raw token")
	}
}
