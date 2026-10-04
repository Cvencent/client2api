package core

import (
	"strings"
	"testing"
)

func TestRedactMasksCredentials(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghij"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain prose is untouched", "no credential here", "no credential here"},
		{"numeric uid is untouched", "3595881099822378", "3595881099822378"},
		{"dashed label is untouched", "cli-login", "cli-login"},
		{"token prefix fallback", "tok:" + jwt, "tok=<redacted>"},
		{"token prefix short tail", "tok:abcdef0123456789", "tok=<redacted>"},
		{"embedded jwt", "failed for token " + jwt, "failed for token <redacted-jwt>"},
		{"bare jwt with no marker", jwt, "<redacted-jwt>"},
		// A regression guard: three dot-separated words are not a JWT, and
		// redacting this hint would make the message useless.
		{
			"dotted config path is untouched",
			`no credential: set clients.qwenwork.access_token, CLIENT2API_QWENWORK_TOKEN, or run the device flow ("login": true)`,
			`no credential: set clients.qwenwork.access_token, CLIENT2API_QWENWORK_TOKEN, or run the device flow ("login": true)`,
		},
		{"dotted module id is untouched", "zcode-config:builtin:bigmodel", "zcode-config:builtin:bigmodel"},
		{"bearer", "Authorization: Bearer " + jwt, "Authorization: Bearer <redacted>"},
		{"api key", `"api_key": "deadbeefdeadbeef"`, `"api_key=<redacted>"`},
		{"cosy key", "cosy-key: AAAAbbbbCCCCdddd", "cosy-key=<redacted>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Redact(tc.in); got != tc.want {
				t.Fatalf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactLeavesNoCredentialBehind(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghij"
	in := "account " + jwt + " parked; key: supersecretvalue; Bearer " + jwt
	got := Redact(in)
	for _, leak := range []string{jwt, "supersecretvalue"} {
		if strings.Contains(got, leak) {
			t.Fatalf("Redact(%q) = %q, still contains %q", in, got, leak)
		}
	}
}

func TestRedactAnyWalksContainers(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghij"
	in := map[string]any{
		"id":    "tok:" + jwt,
		"plain": 42,
		"nested": map[string]any{
			"note": "token: " + jwt,
		},
		"list": []any{"tok:" + jwt, 7},
	}
	got, ok := RedactAny(in).(map[string]any)
	if !ok {
		t.Fatalf("RedactAny returned %T, want map[string]any", RedactAny(in))
	}
	if s := got["id"].(string); s != "tok=<redacted>" {
		t.Errorf("id = %q", s)
	}
	if got["plain"] != 42 {
		t.Errorf("plain = %v, want 42", got["plain"])
	}
	if s := got["nested"].(map[string]any)["note"].(string); strings.Contains(s, jwt) {
		t.Errorf("nested note leaked: %q", s)
	}
	if s := got["list"].([]any)[0].(string); strings.Contains(s, jwt) {
		t.Errorf("list element leaked: %q", s)
	}
	if got["list"].([]any)[1] != 7 {
		t.Errorf("list numeric element = %v, want 7", got["list"].([]any)[1])
	}
	// The input must not be mutated in place.
	if s := in["id"].(string); s != "tok:"+jwt {
		t.Errorf("RedactAny mutated its input: %q", s)
	}
}

func TestRedactStatusCoversEveryRenderedField(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghij"
	st := Status{
		Name:   "qwenwork",
		Detail: "refreshed with Bearer " + jwt,
		Accounts: []AccountStatus{
			{
				ID:    "tok:" + jwt,
				Label: "account " + jwt,
				Note:  "api_key=deadbeefdeadbeef",
				Extra: map[string]any{"source": "token: " + jwt, "count": 3},
			},
		},
	}
	got := RedactStatus(st)
	if strings.Contains(got.Detail, jwt) {
		t.Errorf("Detail leaked: %q", got.Detail)
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("Accounts = %d, want 1", len(got.Accounts))
	}
	a := got.Accounts[0]
	for field, s := range map[string]string{"ID": a.ID, "Label": a.Label, "Note": a.Note} {
		if strings.Contains(s, jwt) || strings.Contains(s, "deadbeefdeadbeef") {
			t.Errorf("%s leaked: %q", field, s)
		}
	}
	if s, _ := a.Extra["source"].(string); strings.Contains(s, jwt) {
		t.Errorf("Extra[source] leaked: %q", s)
	}
	if a.Extra["count"] != 3 {
		t.Errorf("Extra[count] = %v, want 3", a.Extra["count"])
	}
	// A module that reports nothing sensitive must come back unchanged.
	clean := RedactStatus(Status{Name: "trae", Detail: "1 ready", Accounts: []AccountStatus{{ID: "3595881099822378", Label: "用户4661435180"}}})
	if clean.Accounts[0].ID != "3595881099822378" || clean.Accounts[0].Label != "用户4661435180" {
		t.Errorf("benign identifiers were altered: %+v", clean.Accounts[0])
	}
}
