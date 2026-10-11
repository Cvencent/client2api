package autoclaw

import (
	"encoding/json"
	"testing"
)

func TestAccountKey(t *testing.T) {
	const want = "16d1020337662d025778fb501dcb7430ef1f2b19e9fc2405417a1eb0826ef94c"
	if got := accountKey("da46b7a025224a79d29ad9fdf2c03527"); got != want {
		t.Errorf("accountKey = %q, want %q", got, want)
	}
}

func TestSealRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	blob := sealTestBlob([]byte("hello"), key)
	got, err := decryptSafeStorageBlob(blob, key)
	if err != nil || string(got) != "hello" {
		t.Fatalf("round trip: %v %q", err, got)
	}
}

func TestDecryptSafeStorageBlobRejectsShort(t *testing.T) {
	if _, err := decryptSafeStorageBlob([]byte("v10short"), make([]byte, 32)); err == nil {
		t.Fatal("short blob should be rejected")
	}
}

func TestParseDesktopCredentialAcceptsScalarUpdatedAt(t *testing.T) {
	for _, raw := range []string{
		`{"schemaVersion":1,"revision":7,"accessToken":"tok","refreshToken":"rt","updatedAt":1700000000000}`,
		`{"schemaVersion":1,"revision":7,"accessToken":"tok","refreshToken":"rt","updatedAt":"1700000000000"}`,
	} {
		cred, err := parseDesktopCredential([]byte(raw))
		if err != nil {
			t.Fatalf("parseDesktopCredential(%s): %v", raw, err)
		}
		if cred.AccessToken != "tok" || cred.RefreshToken != "rt" {
			t.Fatalf("unexpected credential: %+v", cred)
		}
	}
}
func TestParseCredentialBundle(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"access_token": "tok", "refresh_token": "rt"})
	cred, err := parseCredentialBundle(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cred.AccessToken != "tok" || cred.RefreshToken != "rt" {
		t.Errorf("unexpected credential: %+v", cred)
	}
	bad, _ := json.Marshal(map[string]any{"nope": 1})
	if _, err := parseCredentialBundle(bad); err == nil {
		t.Fatal("bundle without access_token should fail")
	}
}

func TestBundleEntries(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"accounts": []any{map[string]any{"access_token": "a"}, map[string]any{"accessToken": "b"}}})
	entries, err := bundleEntries(raw)
	if err != nil {
		t.Fatalf("bundleEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	single, _ := json.Marshal(map[string]any{"access_token": "x"})
	one, err := bundleEntries(single)
	if err != nil || len(one) != 1 {
		t.Fatalf("single entry bundle: %v %d", err, len(one))
	}
}
