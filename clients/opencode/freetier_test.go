package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The anonymous free-tier handshake.
//
// Zen answers a bare request for a *-free id with
// FreeTierError("OpenCode's free tier can only be used from within OpenCode").
// What its console inspects is the request shape: a streaming chat that
// declares functions named `bash` and `read`.  These tests pin both halves so a
// future refactor cannot quietly drop either and turn every free model into a
// 403 again.
// ---------------------------------------------------------------------------

func decodeFreeBody(t *testing.T, cfg Config, req *core.ChatRequest) map[string]any {
	t.Helper()
	raw, err := buildFreeChatBody(cfg, req)
	if err != nil {
		t.Fatalf("buildFreeChatBody = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the free body is not JSON: %v (%s)", err, raw)
	}
	return got
}

func toolNamesOf(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["tools"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("tools = %T, want a JSON array", raw)
	}
	names := make([]string, 0, len(list))
	for _, entry := range list {
		obj, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("tool entry = %T, want an object", entry)
		}
		fn, _ := obj["function"].(map[string]any)
		name, _ := fn["name"].(string)
		names = append(names, name)
	}
	return names
}

func countName(names []string, want string) int {
	n := 0
	for _, name := range names {
		if name == want {
			n++
		}
	}
	return n
}

func TestFreeChatBodyCarriesTheHandshake(t *testing.T) {
	body := decodeFreeBody(t, Config{}, chatRequest("space-bunny-free"))
	if body["stream"] != true {
		t.Fatalf("stream = %v, want true", body["stream"])
	}
	names := toolNamesOf(t, body)
	if countName(names, "bash") != 1 || countName(names, "read") != 1 {
		t.Fatalf("tools = %v, want exactly one bash and one read", names)
	}
}

func TestPlainChatBodyInjectsNothing(t *testing.T) {
	body := decodeBody(t, Config{}, chatRequest("gpt-5.1"))
	if _, ok := body["tools"]; ok {
		t.Fatalf("tools = %v, want the plain body to carry none", body["tools"])
	}
}

func TestFreeHandshakeKeepsTheCallersOwnTools(t *testing.T) {
	req := chatRequest("space-bunny-free")
	req.Tools = []core.Tool{
		{Name: "bash", Description: "the caller's own bash"},
		{Name: "my_tool", Description: "unrelated"},
	}
	names := toolNamesOf(t, decodeFreeBody(t, Config{}, req))
	if countName(names, "bash") != 1 {
		t.Fatalf("tools = %v, want the caller's bash kept, not duplicated", names)
	}
	if countName(names, "read") != 1 || countName(names, "my_tool") != 1 {
		t.Fatalf("tools = %v, want read added next to my_tool", names)
	}
}

// captureBodyClient records the outbound chat body of the next request.
func captureBodyClient(t *testing.T) (*Client, *[]byte) {
	t.Helper()
	got := new([]byte)
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		if r.Body != nil {
			*got, _ = io.ReadAll(r.Body)
		}
		return sseResponse(happySSE), nil
	})
	return c, got
}

func anonymousAccount() accountRecord {
	return accountRecord{
		ID: "opencode:anonymous", AuthMode: "anonymous", APIKey: "public",
		Enabled: true, Source: sourcePanel,
	}
}

func TestAnonymousChatSendsTheHandshake(t *testing.T) {
	c, got := captureBodyClient(t)
	c.pool.upsert(anonymousAccount())

	stream, err := c.Chat(context.Background(), chatRequest("space-bunny-free"))
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	defer stream.Close()

	var body map[string]any
	if err := json.Unmarshal(*got, &body); err != nil {
		t.Fatalf("the request body is not JSON: %v (%s)", err, *got)
	}
	if body["stream"] != true {
		t.Fatalf("stream = %v, want true", body["stream"])
	}
	names := toolNamesOf(t, body)
	if countName(names, "bash") != 1 || countName(names, "read") != 1 {
		t.Fatalf("tools = %v, want the free-tier handshake", names)
	}
}

func TestOAuthChatIsNotHandshaken(t *testing.T) {
	c, got := captureBodyClient(t)
	c.pool.upsert(accountRecord{
		ID: "opencode:oauth", AuthMode: "oauth", AccessToken: "tok", APIKey: "zen-key",
		Enabled: true, Source: sourcePanel,
	})

	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	defer stream.Close()

	var body map[string]any
	if err := json.Unmarshal(*got, &body); err != nil {
		t.Fatalf("the request body is not JSON: %v (%s)", err, *got)
	}
	if _, ok := body["tools"]; ok {
		t.Fatalf("tools = %v, want a signed-in account left alone", body["tools"])
	}
}

// The Test button probes an anonymous account with a bare request, which the
// vendor refuses — without the handshake every free account would look broken.
func TestAnonymousAccountProbeUsesTheHandshake(t *testing.T) {
	c, got := captureBodyClient(t)
	c.pool.upsert(anonymousAccount())

	res, err := c.TestAccount(context.Background(), "opencode:anonymous")
	if err != nil {
		t.Fatalf("TestAccount = %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount = %+v, want OK", res)
	}
	var body map[string]any
	if err := json.Unmarshal(*got, &body); err != nil {
		t.Fatalf("the probe body is not JSON: %v (%s)", err, *got)
	}
	names := toolNamesOf(t, body)
	if countName(names, "bash") != 1 || countName(names, "read") != 1 {
		t.Fatalf("probe tools = %v, want the free-tier handshake", names)
	}
}

// Every id the vendor publishes at zero cost has to be reachable, not just the
// one the module used to hard-code.
func TestFreeAllowlistCoversTheZeroCostCatalogue(t *testing.T) {
	for _, id := range []string{
		"space-bunny-free",
		"big-pickle",
		"fledge-alpha-free",
		"longcat-2.5-preview-free",
		"mimo-v2.5-free",
		"mimo-v2.6-flash-free",
		"nemotron-3-ultra-free",
		"nemotron-3.5-lightning-free",
	} {
		if !anonymousModelAllowed(id, Config{}) {
			t.Errorf("anonymousModelAllowed(%q) = false, want true", id)
		}
	}
}

// The served catalogue stays narrowed to the allowlist, and it now carries the
// whole zero-cost set rather than a single id.
func TestAnonymousOnlyPoolServesTheWholeFreeSet(t *testing.T) {
	live := `{"object":"list","data":[{"id":"space-bunny-free"},{"id":"big-pickle"},{"id":"mimo-v2.5-free"},{"id":"gpt-5.1"}]}`
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, live), nil
	})
	c.pool.upsert(anonymousAccount())

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if containsModel(models, "gpt-5.1") {
		t.Fatalf("models = %v, want the paid model dropped", modelIDsOf(models))
	}
	for _, want := range []string{"space-bunny-free", "big-pickle", "mimo-v2.5-free"} {
		if !containsModel(models, want) {
			t.Errorf("models = %v, want %q served", modelIDsOf(models), want)
		}
	}
}

// ---------------------------------------------------------------------------
// The session header.
//
// The handshake body is only half of it: the console also refuses a request
// with no `x-opencode-session` header, and accepts one whose value is `ses_`
// followed by 12 lowercase hex characters and 14 base62 characters.  These
// tests pin the header onto the anonymous path only.
// ---------------------------------------------------------------------------

var freeSessionShape = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func TestAnonymousChatSendsTheFreeTierSession(t *testing.T) {
	seen := new(http.Header)
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		*seen = r.Header.Clone()
		return sseResponse(happySSE), nil
	})
	c.pool.upsert(anonymousAccount())

	stream, err := c.Chat(context.Background(), chatRequest("space-bunny-free"))
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	defer stream.Close()

	sid := seen.Get("x-opencode-session")
	if !freeSessionShape.MatchString(sid) {
		t.Fatalf("x-opencode-session = %q, want ses_ plus 12 hex + 14 base62", sid)
	}
}

func TestOAuthChatSendsNoFreeTierSession(t *testing.T) {
	seen := new(http.Header)
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		*seen = r.Header.Clone()
		return sseResponse(happySSE), nil
	})
	c.pool.upsert(accountRecord{
		ID: "opencode:oauth", AuthMode: "oauth", AccessToken: "tok", APIKey: "zen-key",
		Enabled: true, Source: sourcePanel,
	})

	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	defer stream.Close()

	if sid := seen.Get("x-opencode-session"); sid != "" {
		t.Fatalf("x-opencode-session = %q, want a signed-in account left alone", sid)
	}
}

// The minted id must be stable within a process and must satisfy the shape the
// vendor validates, or every anonymous request becomes a 403 again.
func TestFreeTierSessionIsWellFormedAndStable(t *testing.T) {
	c := newTestClient(t, Config{})
	first := c.freeTierSession()
	if !freeSessionShape.MatchString(first) {
		t.Fatalf("freeTierSession() = %q, want ses_ plus 12 hex + 14 base62", first)
	}
	if again := c.freeTierSession(); again != first {
		t.Fatalf("freeTierSession() = %q then %q, want one id per process", first, again)
	}
}

// ---------------------------------------------------------------------------
// The User-Agent.
//
// The third leg of the gate: the console only lets the anonymous credential
// through when the User-Agent names the opencode client at version 1.18 or
// newer.  A bare `opencode`, a different product name, or an older version is
// refused with the same FreeTierError.  Pin the constant and the header.
// ---------------------------------------------------------------------------

var freeUserAgentShape = regexp.MustCompile(`^opencode/(\d+)\.(\d+)`)

func TestUserAgentSatisfiesTheFreeTierGate(t *testing.T) {
	m := freeUserAgentShape.FindStringSubmatch(userAgent)
	if m == nil {
		t.Fatalf("userAgent = %q, want opencode/<version>", userAgent)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 1 || (major == 1 && minor < 18) {
		t.Fatalf("userAgent = %q, want version 1.18 or newer", userAgent)
	}
}

func TestAnonymousChatSendsTheGateUserAgent(t *testing.T) {
	seen := new(http.Header)
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		*seen = r.Header.Clone()
		return sseResponse(happySSE), nil
	})
	c.pool.upsert(anonymousAccount())

	stream, err := c.Chat(context.Background(), chatRequest("space-bunny-free"))
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	defer stream.Close()

	if ua := seen.Get("User-Agent"); ua != userAgent {
		t.Fatalf("User-Agent = %q, want %q", ua, userAgent)
	}
}
