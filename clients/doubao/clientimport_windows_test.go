//go:build windows

package doubao

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"os"
	"testing"
)

// TestStripAppBoundHeader pins the Chromium 127+ cookie fix: the app-bound
// header is a 32-byte binary prefix inside the AES-GCM plaintext, and the
// cookie text that follows must survive it.  Without this strip the replayed
// session is rejected as "invalid user".
func TestStripAppBoundHeader(t *testing.T) {
	header := make([]byte, appBoundHeaderSize)
	for i := range header {
		header[i] = 0xAB
	}
	blob := append(append([]byte(nil), header...), []byte("040ccb942b6a13")...)
	if got := stripAppBoundHeader(blob); got != "040ccb942b6a13" {
		t.Fatalf("stripAppBoundHeader = %q", got)
	}
	// A value that genuinely has no header must be returned untouched.
	if got := stripAppBoundHeader([]byte("v14_value")); got != "v14_value" {
		t.Fatalf("plaintext with no header = %q", got)
	}
}

// TestCookieHeaderFromRows pins the request-header rendering: duplicates keep
// the last value and non-header values are dropped.
func TestCookieHeaderFromRows(t *testing.T) {
	rows := []cookieRow{
		{name: "sessionid", value: "old"},
		{name: "ttwid", value: "t1"},
		{name: "sessionid", value: "new"},
		{name: "binary", value: "a\x00b"},
	}
	got := cookieHeaderFromRows(rows)
	want := "sessionid=new; ttwid=t1"
	if got != want {
		t.Fatalf("cookieHeaderFromRows = %q, want %q", got, want)
	}
}

// TestDecryptCookieValueRoundTrip pins the value format: a v10 envelope is
// nonce||ciphertext||tag over the app-bound header plus the cookie text, and
// decrypting it must hand back exactly the cookie text.
func TestDecryptCookieValueRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	plaintext := append(make([]byte, appBoundHeaderSize), []byte("040ccb942b6a13deadbeef")...)
	blob := append([]byte("v10"), nonce...)
	blob = gcm.Seal(blob, nonce, plaintext, nil)

	got, err := decryptCookieValue(blob, key)
	if err != nil {
		t.Fatalf("decryptCookieValue: %v", err)
	}
	if got != "040ccb942b6a13deadbeef" {
		t.Fatalf("decrypted cookie = %q", got)
	}
}

// TestSQLiteReaderSmoke opens the real client database when this machine has
// one, to prove the winsqlite3 binding end to end.  It is a smoke test: it
// skips wherever the client is not installed and never prints a value.
func TestSQLiteReaderSmoke(t *testing.T) {
	dir := (&config{}).clientUserDataDir()
	if dir == "" {
		t.Skip("no user-data directory on this machine")
	}
	var path string
	for _, profile := range []string{"Default", "Profile 1"} {
		for _, p := range cookieDBPaths(dir + string(os.PathSeparator) + profile) {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
		if path != "" {
			break
		}
	}
	if path == "" {
		t.Skip("no Doubao client cookie database on this machine")
	}
	db, err := openSQLite(path)
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	defer closeSQLite(db)
	rows, err := db.queryRows("select count(*) from cookies")
	if err != nil {
		t.Fatalf("queryRows: %v", err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("count(*) returned %d rows", len(rows))
	}
}
