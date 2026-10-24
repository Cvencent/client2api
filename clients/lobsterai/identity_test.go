package lobsterai

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding
	return string(header) + "." + enc.EncodeToString(payload) + ".sig"
}

func TestDecodeNestedCredentialsKeepsVendorIdentity(t *testing.T) {
	raw := []byte(`{
		"auth": {"access_token":"access-1","refresh_token":"refresh-1","first_keyfrom":"bing","latest_keyfrom":"official"},
		"account": {"uid":777,"userId":"u-777","yid":"yid-777","nickname":"Lobster One"}
	}`)
	accts, err := decodeCredentialsBody(raw)
	if err != nil {
		t.Fatalf("decodeCredentialsBody: %v", err)
	}
	if len(accts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accts))
	}
	got := accts[0]
	if got.UID != "777" || got.UserID != "u-777" || got.YID != "yid-777" {
		t.Errorf("identity = uid %q, user_id %q, yid %q; want 777/u-777/yid-777", got.UID, got.UserID, got.YID)
	}
	if got.Nickname != "Lobster One" {
		t.Errorf("nickname = %q", got.Nickname)
	}
}

func TestLoadCredentialsRecoversYIDFromAccessToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, credentialsFile)
	token := testJWT(t, map[string]any{"sub": "777", "yid": "yid-777"})
	if err := saveCredentials(path, []accountRecord{{
		ID:            "lobsterai:777",
		UID:           "777",
		UserID:        "777",
		AccessToken:   token,
		FirstKeyfrom:  "bing",
		LatestKeyfrom: "official",
		AddedAt:       "2026-10-14T00:10:05Z",
	}}); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}

	got := loadCredentials(path)
	if len(got) != 1 {
		t.Fatalf("loadCredentials returned %d accounts, want 1", len(got))
	}
	if got[0].YID != "yid-777" {
		t.Errorf("yid = %q, want the yid claim from the access token", got[0].YID)
	}
}

func TestAccountFromConfigReplacesTimestampKeyfrom(t *testing.T) {
	rec := accountFromConfig(AccountConfig{
		AccessToken:   "access-1",
		FirstKeyfrom:  "1791907775209",
		LatestKeyfrom: "1791907805311",
	}, time.Now())
	if rec.FirstKeyfrom != defaultFirstKeyfrom || rec.LatestKeyfrom != defaultLatestKeyfrom {
		t.Errorf("accountFromConfig kept timestamp keyfrom: %+v", rec)
	}
}
