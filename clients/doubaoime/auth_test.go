package doubaoime

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"client2api/internal/core"
)

func TestRefreshCredentialsReusesCookieJar(t *testing.T) {
	var registerCalls, settingsCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		registerCalls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("register method = %s", r.Method)
		}
		if got := r.URL.Query().Get("aid"); got != "401734" {
			t.Errorf("aid = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("register body: %v", err)
		}
		header, _ := body["header"].(map[string]any)
		if header["cdid"] == "" || header["openudid"] == "" || header["clientudid"] == "" {
			t.Errorf("identity fields missing: %#v", header)
		}
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "cookie-1", Path: "/"})
		fmt.Fprint(w, `{"device_id":123456}`)
	})
	mux.HandleFunc("/settings", func(w http.ResponseWriter, r *http.Request) {
		settingsCalls.Add(1)
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "sid=cookie-1") {
			t.Errorf("settings cookie = %q", got)
		}
		if got := r.Header.Get("x-ss-stub"); got != strings.ToUpper(md5Hex("body=null")) {
			t.Errorf("x-ss-stub = %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.Form.Get("body"); got != "null" {
			t.Errorf("body = %q", got)
		}
		fmt.Fprint(w, `{"data":{"settings":{"asr_config":{"app_key":"key-1"}}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, fmt.Sprintf(`{"register_url":%q,"settings_url":%q}`, srv.URL+"/register", srv.URL+"/settings"))
	if err := c.ensureCredentials(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	first := c.credentialsSnapshot()
	if first.DeviceID != "123456" || first.AppKey != "key-1" || !first.valid() {
		t.Fatalf("credentials = %+v", first)
	}
	if err := c.ensureCredentials(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if registerCalls.Load() != 1 || settingsCalls.Load() != 1 {
		t.Fatalf("calls = register %d settings %d, want one of each", registerCalls.Load(), settingsCalls.Load())
	}

	var raw cachedCredentials
	if err := core.ReadJSON(c.credentialsPath, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.AppKey != "key-1" {
		t.Fatalf("persisted credentials = %+v", raw)
	}
}

func TestNewIdentityFormats(t *testing.T) {
	id, err := newIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if len(id.OpenUDID) != 16 {
		t.Fatalf("openudid = %q", id.OpenUDID)
	}
	if len(id.CDID) != 36 || len(id.ClientUDID) != 36 {
		t.Fatalf("uuid lengths = %d/%d", len(id.CDID), len(id.ClientUDID))
	}
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
