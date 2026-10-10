package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
	"client2api/internal/gateway"
	"client2api/internal/livecfg"
	"client2api/internal/panel"
)

func TestLoadConfigRejectsSourceCredentialsAndUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"sources":{"relay":{"base_url":"https://relay.example/v1","api_key":"plainopaqueabc123"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConfigAt(path, "test", false); err == nil {
		t.Fatal("source credentials bypassed strict schema at startup")
	}
}

func TestDynamicSourceUnifiedGatewayAndBundleRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-demo-key" {
			http.Error(w, "wrong upstream key", 401)
			return
		}
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"vendor/demo"}]}`)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "vendor/demo" {
			t.Errorf("lost model slash: %v", body["model"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"source reply\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	dataDir := filepath.Join(dir, "data")
	if err := os.WriteFile(configPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	registry := core.NewRegistry()
	deps := core.Deps{DataDir: dataDir, HTTPClient: upstream.Client()}
	reload := func() error {
		cfg, _, err := loadConfigAt(configPath, "test", false)
		if err != nil {
			return err
		}
		if err := registry.ReconcileSources(cfg.Sources, deps); err != nil {
			return err
		}
		registry.SetPlatformConfigs(cfg.platformConfigsFor(registry.All()))
		registry.SetModelGroups(cfg.modelGroups())
		return nil
	}
	live := livecfg.New(livecfg.Snapshot{APIKey: "unified-inbound-key"})
	handler := panel.New(panel.Options{Registry: registry, ConfigPath: configPath, DataDir: dataDir, Reload: reload, Live: live})
	srv := httptest.NewServer(gateway.NewServer(gateway.Options{Registry: registry, Live: live, Panel: handler}).Handler)
	defer srv.Close()
	request := func(method, path, body string, code int) map[string]any {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer unified-inbound-key")
		req.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != code {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	def, _ := json.Marshal(core.SourceConfig{Label: "Relay", BaseURL: upstream.URL + "/v1"})
	request("PUT", "/panel/api/sources/my-relay", string(def), 200)
	request("POST", "/panel/api/clients/my-relay/accounts", `{"fields":{"api_key":"upstream-demo-key"}}`, 200)
	request("POST", "/panel/api/models/refresh?client=my-relay", `{}`, 200)
	request("PATCH", "/panel/api/config", `{"platforms":{"my-relay":{"priority":-5}},"model_groups":{"shared":{"members":["my-relay/vendor/demo"]}}}`, 200)
	if err := reload(); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"my-relay/vendor/demo", "vendor/demo", "shared"} {
		body, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
		out := request("POST", "/v1/chat/completions", string(body), 200)
		if !strings.Contains(fmt.Sprint(out["choices"]), "source reply") {
			t.Fatalf("no normalized reply: %v", out)
		}
	}
	resp, err := srv.Client().Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unified key bypassed: %d", resp.StatusCode)
	}
	bundle := request("GET", "/panel/api/bundle", "", 200)
	request("DELETE", "/panel/api/sources/my-relay", "", 200)
	if _, ok := registry.Get("my-relay"); ok {
		t.Fatal("source deletion did not remove route")
	}
	raw, _ := json.Marshal(bundle)
	restored := request("POST", "/panel/api/bundle", string(raw), 200)
	if restored["reloaded"] != true {
		t.Fatalf("source restore was not live: %v", restored)
	}
	if restored["restart_required"] != false {
		t.Fatalf("source-only hot restore unnecessarily required restart: %v", restored)
	}
	if c, ok := registry.Get("my-relay"); !ok || len(c.Status(nil).Models) != 1 {
		t.Fatal("bundle lost source model/account state")
	}
	disk, err := os.ReadFile(filepath.Join(dataDir, "my-relay", "accounts.json"))
	if err != nil || !bytes.Contains(disk, []byte("upstream-demo-key")) {
		t.Fatal("bundle lost isolated source store")
	}
}
