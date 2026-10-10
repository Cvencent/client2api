package qoder

import (
	"context"
	"crypto/md5"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// TestCosyPublicKeyMatchesVendorPEM proves the compiled modulus and exponent
// are the vendor's own key: a typo in cosyModulusHex would otherwise produce a
// signature every request gets rejected for, with no local symptom.
func TestCosyPublicKeyMatchesVendorPEM(t *testing.T) {
	block, _ := pem.Decode([]byte(cosyVendorPublicKeyPEM))
	if block == nil {
		t.Fatal("failed to decode the embedded vendor public key PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("ParsePKIXPublicKey: %v", err)
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("embedded key is %T, want *rsa.PublicKey", parsed)
	}
	got, err := cosyPublicKey()
	if err != nil {
		t.Fatalf("cosyPublicKey: %v", err)
	}
	if got.N.Cmp(pub.N) != 0 || got.E != pub.E {
		t.Fatalf("built-in modulus does not match the vendor PEM")
	}
	if hex.EncodeToString(pub.N.Bytes()) != cosyModulusHex {
		t.Fatalf("cosyModulusHex does not match the vendor PEM")
	}
}

// TestPathForSignatureStripsOnlyAlgoSegment pins the signing path rule: the
// /algo edge mount is removed, an unrelated /algoish prefix is not.
func TestPathForSignatureStripsOnlyAlgoSegment(t *testing.T) {
	cases := map[string]string{
		"https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1": "/api/v2/model/list",
		"https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/x":   "/api/v2/service/pro/sse/x",
		"https://gateway.qoder.com.cn/algoish/api":                     "/algoish/api",
		"https://gateway.qoder.com.cn/algo":                            "/",
		"https://gateway.qoder.com.cn/api/v2/x":                        "/api/v2/x",
	}
	for raw, want := range cases {
		got, err := pathForSignature(raw)
		if err != nil {
			t.Fatalf("pathForSignature(%q): %v", raw, err)
		}
		if got != want {
			t.Errorf("pathForSignature(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestCosySignatureUsesNewlineJoinedFields re-derives the digest independently
// with crypto/md5, so a change to the field order or a separator cannot pass.
func TestCosySignatureUsesNewlineJoinedFields(t *testing.T) {
	payload, key, body, path := "PAYLOAD", "KEY", `{"a":1}`, "/api/v2/x"
	date := int64(1700000000)
	sum := md5.Sum([]byte(payload + "\n" + key + "\n" + "1700000000" + "\n" + body + "\n" + path))
	want := hex.EncodeToString(sum[:])
	if got := cosySignature(payload, key, date, body, path); got != want {
		t.Fatalf("cosySignature = %q, want %q", got, want)
	}
}

// TestCosyHeadersSignTheStrippedPath pins the whole header set together: the
// Authorization suffix must match a digest over the /algo-stripped path, not
// the raw one.
func TestCosyHeadersSignTheStrippedPath(t *testing.T) {
	sess := cosySession{uid: "u-1", tempKey: "0123456789abcdef", cosyKey: "COSYKEY", info: "INFO"}
	raw := "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation"
	body := `{"model":"auto"}`
	hdrs, err := sess.headers(cosyRequest{
		UID:        "u-1",
		Body:       body,
		RawURL:     raw,
		Accept:     "text/event-stream",
		RequestID:  "req-1",
		XRequestID: "xreq-1",
		UnixDate:   1700000000,
	})
	if err != nil {
		t.Fatalf("headers: %v", err)
	}
	auth := hdrs["authorization"]
	const prefix = "Bearer COSY."
	if !strings.HasPrefix(auth, prefix) {
		t.Fatalf("authorization = %q, want a COSY bearer", auth)
	}
	parts := strings.Split(strings.TrimPrefix(auth, prefix), ".")
	if len(parts) != 2 {
		t.Fatalf("authorization = %q, want payload.signature", auth)
	}
	payload, sig := parts[0], parts[1]
	want := cosySignature(payload, sess.cosyKey, 1700000000, body, "/api/v2/service/pro/sse/agent_chat_generation")
	if sig != want {
		t.Fatalf("signature = %q, want %q", sig, want)
	}
	if hdrs["cosy-user"] != "u-1" || hdrs["cosy-key"] != "COSYKEY" || hdrs["cosy-date"] != "1700000000" {
		t.Fatalf("COSY identity headers are wrong: %#v", hdrs)
	}
}

// TestGatewayChatModelsKeepsChatGroupAndDedupes pins the catalogue rules: only
// the chat group is read, disabled entries are dropped, duplicates collapse.
func TestGatewayChatModelsKeepsChatGroupAndDedupes(t *testing.T) {
	raw := `{"chat":[` +
		`{"key":"auto","display_name":"Auto","enable":true,"is_default":true,"is_reasoning":true,"max_input_tokens":200000},` +
		`{"key":"auto","display_name":"Auto dup","enable":true,"max_input_tokens":1},` +
		`{"key":"retired","display_name":"Retired","enable":false,"max_input_tokens":999},` +
		`{"key":"gfmodel","display_name":"GLM-5.3-Flash","enable":true,"max_input_tokens":1000000}` +
		`],"developer":[{"key":"ignored","display_name":"Ignored","enable":true}]}`
	var list gatewayModelList
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := list.chatModels()
	if len(got) != 2 {
		t.Fatalf("chatModels = %#v, want two entries", got)
	}
	if got[0].ID != "auto" || got[1].ID != "gfmodel" {
		t.Fatalf("chatModels order = %q, %q; want auto, gfmodel", got[0].ID, got[1].ID)
	}
	if got[0].ContextLength != 200000 || got[1].ContextLength != 1000000 {
		t.Fatalf("context lengths = %d, %d", got[0].ContextLength, got[1].ContextLength)
	}
	if !got[0].Default || !got[0].Reasoning {
		t.Fatalf("auto lost its flags: %#v", got[0])
	}

	cat := catalogue(got)
	if cat[0].Extra["context_length"] != 200000 {
		t.Fatalf("catalogue dropped the vendor context: %#v", cat[0].Extra)
	}
}

// TestChatBodyCarriesUsageAndSessionContext pins the body the gateway is signed
// over: stream_options must ask for usage, and metadata.context must carry the
// caller's session ids so the vendor attributes the spend correctly.
func TestChatBodyCarriesUsageAndSessionContext(t *testing.T) {
	req := &core.ChatRequest{
		Model:                 "auto",
		ConversationID:        "sess-1",
		ConversationRequestID: "turn-1",
		Messages:              []core.Message{{Role: "user", Content: "hi"}},
	}
	body, err := chatBody(req, "auto")
	if err != nil {
		t.Fatalf("chatBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if obj["stream"] != true {
		t.Fatalf("stream = %v, want true", obj["stream"])
	}
	opts, _ := obj["stream_options"].(map[string]any)
	if opts["include_usage"] != true {
		t.Fatalf("stream_options = %#v, want include_usage", obj["stream_options"])
	}
	meta, _ := obj["metadata"].(map[string]any)
	ctx, _ := meta["context"].(map[string]any)
	if ctx["session_id"] != "sess-1" || ctx["request_id"] != "turn-1" {
		t.Fatalf("metadata.context = %#v, want the caller's ids", ctx)
	}
	if ctx["client_type"] != chatClientType {
		t.Fatalf("client_type = %v, want %q", ctx["client_type"], chatClientType)
	}
}

// probeSSEFrame is the gateway's real envelope: a 200 SSE frame whose body
// string holds an OpenAI chunk, then the [DONE] terminator.
const probeSSEFrame = "data:{\"headers\":{\"Content-Type\":[\"application/json\"]},\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"pong\\\"}}]}\",\"statusCodeValue\":200,\"statusCode\":\"OK\"}\n\n" +
	"data:{\"body\":\"[DONE]\",\"statusCodeValue\":200,\"statusCode\":\"OK\"}\n\n"

// TestTestAccountSendsARealChatRequest pins the change the operator asked for:
// the panel's 测试 button must hit the chat route and report the model's reply,
// not just read the account profile.
func TestTestAccountSendsARealChatRequest(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(probeSSEFrame))
	}))
	defer server.Close()

	acc := account{storedAccount: storedAccount{
		ID: "a1", Token: "dt-token", UserID: "u-1", Name: "tester", Enabled: true,
	}}
	c := &Client{
		cfg:      config{GatewayBase: server.URL},
		up:       newUpstream(config{OpenAPIBase: server.URL}, server.Client(), nil),
		store:    &store{all: []*account{&acc}},
		sessions: map[string]sessionEntry{},
	}

	res, err := c.TestAccount(context.Background(), "a1")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount failed: %+v", res)
	}
	if res.Reply != "pong" {
		t.Fatalf("Reply = %q, want the streamed text", res.Reply)
	}
	if gotPath != "/algo/api/v2/service/pro/sse/agent_chat_generation" {
		t.Fatalf("path = %q, want the chat route", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer COSY.") {
		t.Fatalf("Authorization = %q, want a COSY bearer", gotAuth)
	}
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("the probe body was not JSON: %v", err)
	}
	if sent["model"] != "auto" {
		t.Fatalf("probe model = %v, want auto", sent["model"])
	}

	// A successful probe must clear the account's penalties, exactly like a
	// successful balance read.
	stored, _ := c.store.lookup("a1")
	if stored.dead || !stored.cooldownTill.IsZero() {
		t.Fatalf("successful probe left penalties behind: %+v", stored)
	}
}

// TestProbeChatReportsAnEmptyAnswer keeps the probe honest: an upstream 200
// that streams no text is a failure, not a pass.
func TestProbeChatReportsAnEmptyAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data:{\"body\":\"{}\",\"statusCodeValue\":200}\n\ndata:{\"body\":\"[DONE]\"}\n\n"))
	}))
	defer server.Close()

	acc := account{storedAccount: storedAccount{ID: "a1", Token: "dt", UserID: "u", Enabled: true}}
	c := &Client{
		cfg:      config{GatewayBase: server.URL},
		up:       newUpstream(config{OpenAPIBase: server.URL}, server.Client(), nil),
		store:    &store{all: []*account{&acc}},
		sessions: map[string]sessionEntry{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.probeChat(ctx, acc); err == nil {
		t.Fatal("an empty stream was reported as a successful probe")
	}
}
