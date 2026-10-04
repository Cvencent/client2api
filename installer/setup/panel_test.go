//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPanelURLFromListen(t *testing.T) {
	cases := []struct {
		listen string
		want   string
		ok     bool
	}{
		{listen: "127.0.0.1:8790", want: "http://127.0.0.1:8790/panel/", ok: true},
		{listen: "localhost:8123", want: "http://localhost:8123/panel/", ok: true},
		{listen: ":8788", want: "http://127.0.0.1:8788/panel/", ok: true},
		{listen: "0.0.0.0:9000", want: "http://127.0.0.1:9000/panel/", ok: true},
		{listen: "[::1]:8790", want: "http://[::1]:8790/panel/", ok: true},
		{listen: "[::]:9000", want: "http://127.0.0.1:9000/panel/", ok: true},
		{listen: "", ok: false},
		{listen: "127.0.0.1", ok: false},
		{listen: "not-a-host:port", ok: false},
	}
	for _, tc := range cases {
		got, ok := panelURLFromListen(tc.listen)
		if got != tc.want || ok != tc.ok {
			t.Errorf("panelURLFromListen(%q) = %q,%v; want %q,%v", tc.listen, got, ok, tc.want, tc.ok)
		}
	}
}

func TestConfiguredPanelURLReadsTheInstalledConfig(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "configs", "client2api.json")
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(`{"listen":"127.0.0.1:9123"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := configuredPanelURL(dir)
	if want := "http://127.0.0.1:9123/panel/"; !ok || got != want {
		t.Fatalf("configuredPanelURL = %q,%v; want %q,true", got, ok, want)
	}
}

func TestConfiguredPanelURLRefusesAnInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "configs", "client2api.json")
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(`{"listen":"invalid"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if got, ok := configuredPanelURL(dir); ok {
		t.Fatalf("configuredPanelURL returned %q for an invalid listen address", got)
	}
}
