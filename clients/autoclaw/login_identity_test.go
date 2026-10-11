package autoclaw

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestAuthRequestsCarryDesktopIdentity(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case pathAgentSendCode:
			_, _ = w.Write([]byte(`{"code":0,"data":{"result":true}}`))
		case pathAgentLogin:
			_, _ = w.Write([]byte(`{"code":0,"data":{"user_id":"u","access_token":"a","refresh_token":"r","user_name":"n","user_phone":"17312343429"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv(envUserData, t.TempDir())
	c := &Client{cfg: config{BaseURL: srv.URL}, httpc: srv.Client(), now: time.Now, logins: map[string]*loginSession{}}
	if err := c.sendVerificationCode(t.Context(), "17312343429"); err != nil {
		t.Fatalf("sendVerificationCode: %v", err)
	}
	if _, err := c.signInWithCode(t.Context(), "17312343429", "123456"); err != nil {
		t.Fatalf("signInWithCode: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	for i, body := range bodies {
		if got := body["source_id"]; got != "autoclaw" {
			t.Fatalf("request %d source_id = %#v", i, got)
		}
		device, _ := body["device_id"].(string)
		if device == "" {
			t.Fatalf("request %d has no device_id: %#v", i, body)
		}
	}
	if bodies[0]["device_id"] != bodies[1]["device_id"] {
		t.Fatalf("device_id changed between requests: %#v %#v", bodies[0]["device_id"], bodies[1]["device_id"])
	}
	if bodies[0]["phone"] != "17312343429" {
		t.Fatalf("send-code phone = %#v", bodies[0]["phone"])
	}
	if bodies[1]["phone"] != "17312343429" || bodies[1]["code"] != float64(123456) {
		t.Fatalf("login body = %#v", bodies[1])
	}
}

func TestDeviceIdentityIsStableAndPersisted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envUserData, dir)
	first := deviceIdentity()
	if first == "" {
		t.Fatal("deviceIdentity returned empty")
	}
	second := deviceIdentity()
	if first != second {
		t.Fatalf("deviceIdentity changed: %q -> %q", first, second)
	}
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(deviceFile)))
	if err != nil {
		t.Fatalf("read device file: %v", err)
	}
	var doc deviceDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode device file: %v", err)
	}
	if doc.SchemaVersion != 1 || doc.DeviceID != first {
		t.Fatalf("device file = %+v, want id %q", doc, first)
	}
}

var _ core.Client = (*Client)(nil)
