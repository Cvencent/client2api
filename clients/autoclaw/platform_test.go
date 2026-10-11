package autoclaw

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAutoclawSign(t *testing.T) {
	got := autoclawSign("1700000000")
	if len(got) != 32 {
		t.Fatalf("sign length = %d", len(got))
	}
	if got != autoclawSign("1700000000") {
		t.Fatal("not deterministic")
	}
	if got == autoclawSign("1700000001") {
		t.Fatal("timestamp should change the sign")
	}
}

func TestManagedIdentityHeaders(t *testing.T) {
	h := managedIdentityHeaders("2.0.5")
	for _, key := range []string{"X-Version", "X-Tm", "X-Product", "X-Auth-Appid", "X-Auth-TimeStamp", "X-Auth-Sign", "X-Lang", "X-Channel"} {
		if strings.TrimSpace(h.Get(key)) == "" {
			t.Errorf("header %s is empty", key)
		}
	}
	if h.Get("X-Product") != "autoclaw" || h.Get("X-Auth-Appid") != "100003" {
		t.Error("wrong product identity")
	}
	if h.Get("X-Auth-Sign") != autoclawSign(h.Get("X-Auth-TimeStamp")) {
		t.Error("sign mismatch")
	}
}

func TestChatHeaders(t *testing.T) {
	cred := credential{AccessToken: "abc123"}
	h := chatHeaders(cred, "zai_auto", "sess-1", "req-1", "2.0.5")
	if h.Get("Authorization") != "" {
		t.Error("chat must not carry Authorization")
	}
	if got := h.Get("X-Authorization"); got != "Bearer abc123" {
		t.Errorf("X-Authorization = %q", got)
	}
	if h.Get("X-Session-Id") != "sess-1" {
		t.Errorf("X-Session-Id = %q", h.Get("X-Session-Id"))
	}
	if h.Get("X-Request-Model") != "zai_auto" {
		t.Errorf("X-Request-Model = %q", h.Get("X-Request-Model"))
	}
	if h.Get("X-Trace-Id") != "req-1" {
		t.Error("trace id missing")
	}
	if h.Get("x_trace_id") != "autoclaw-desktop" {
		t.Error("x_trace_id missing")
	}
}

func TestChatHeadersPreserveBearer(t *testing.T) {
	cred := credential{AccessToken: "Bearer abc"}
	h := chatHeaders(cred, "m", "s", "r", "")
	if got := h.Get("X-Authorization"); got != "Bearer abc" {
		t.Errorf("X-Authorization = %q", got)
	}
}

func TestEnvelopeDecode(t *testing.T) {
	bad, _ := json.Marshal(map[string]any{"code": 810000, "msg": "out of credits"})
	if _, err := decodeEnvelope(http.StatusOK, bad); err == nil {
		t.Fatal("business error should surface")
	}
	ok, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]int{"x": 1}})
	if _, err := decodeEnvelope(http.StatusOK, ok); err != nil {
		t.Fatalf("success envelope: %v", err)
	}
}

func TestNormalizePhone(t *testing.T) {
	if got := normalizePhone(" 173-1234-3429 "); got != "17312343429" {
		t.Errorf("normalizePhone = %q", got)
	}
}

func TestNormalizeWalletScope(t *testing.T) {
	if got := normalizeWalletScope("wallet_scope_long_term"); got != "long_term" {
		t.Errorf("scope = %q", got)
	}
	if got := normalizeWalletScope("Daily"); got != "daily" {
		t.Errorf("scope = %q", got)
	}
}

func TestMaskPhone(t *testing.T) {
	if got := maskPhone("17312343429"); got != "173****3429" {
		t.Errorf("maskPhone = %q", got)
	}
}
