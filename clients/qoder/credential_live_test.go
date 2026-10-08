package qoder

import (
	"encoding/json"
	"os"
	"testing"
)

func TestLiveDesktopCredentialDecrypts(t *testing.T) {
	if os.Getenv("CLIENT2API_QODER_LIVE_CREDENTIAL_TEST") != "1" {
		t.Skip("set CLIENT2API_QODER_LIVE_CREDENTIAL_TEST=1 to read the local Qoder CN credential")
	}
	path := localCredentialPath()
	if path == "" {
		t.Skip("no local Qoder CN credential path")
	}
	raw, err := readDesktopCredential(path)
	if err != nil {
		t.Fatalf("readDesktopCredential: %v", err)
	}
	var payload desktopCredential
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decrypted credential is not JSON: %v", err)
	}
	if payload.Token == "" {
		t.Fatal("decrypted credential has no device token")
	}
}
