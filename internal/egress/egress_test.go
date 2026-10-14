package egress

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigNormalizesAndRejectsUnsupportedClientSchemes(t *testing.T) {
	cfg := (Config{Enabled: true, Mode: "client", URL: "ftp://proxy.example:1080"}).Normalize()
	if cfg.Mode != "client" {
		t.Fatalf("Normalize changed mode to %q", cfg.Mode)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted an unsupported proxy scheme")
	}

	cfg.URL = "socks5h://127.0.0.1:1080"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected socks5h: %v", err)
	}
}

func TestConfigRejectsSubscriptionWithoutURL(t *testing.T) {
	cfg := (Config{Enabled: true, Mode: "subscription"}).Normalize()
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted an enabled subscription without a URL")
	}
}

func TestRenderMihomoConfigContainsProviderAndHealthCheck(t *testing.T) {
	body := renderMihomoConfig(mihomoOptions{
		Subscription: "https://subscription.example/token",
		MixedPort:    18080,
		APIPort:      18081,
		Secret:       "controller-secret",
		Auth:         "client2api:listener-secret",
		LogFile:      `C:\data\egress\mihomo.log`,
	})
	for _, want := range []string{
		"mixed-port: 18080",
		"bind-address: 127.0.0.1",
		"allow-lan: false",
		"authentication:",
		"  - \"client2api:listener-secret\"",
		"external-controller: 127.0.0.1:18081",
		"secret: \"controller-secret\"",
		"proxy-providers:",
		"  egress:",
		"    url: \"https://subscription.example/token\"",
		"      url: \"http://www.gstatic.com/generate_204\"",
		"      expected-status: 204",
		"  - name: ofm-outlet",
		"      - egress",
		"  - MATCH,ofm-outlet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered mihomo config is missing %q", want)
		}
	}
}

func TestClientModeStartReturnsConfiguredProxyURL(t *testing.T) {
	cfg := Config{Enabled: true, Mode: "client", URL: "http://user:pass@127.0.0.1:18080"}
	m, err := Start(context.Background(), cfg, t.TempDir(), func(string, ...any) {})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()

	if got := m.Outlet(); got != cfg.URL {
		t.Fatalf("Outlet() = %q, want %q", got, cfg.URL)
	}
}

func TestSubscriptionModeReportsMissingMihomoBinary(t *testing.T) {
	cfg := Config{Enabled: true, Mode: "subscription", URL: "https://subscription.example/token",
		MihomoPath: filepath.Join(t.TempDir(), "missing-mihomo")}
	_, err := Start(context.Background(), cfg, t.TempDir(), func(string, ...any) {})
	if err == nil {
		t.Fatal("Start succeeded with a missing mihomo binary")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error = %v, want an explicit missing-path error", err)
	}
}

func TestFindMihomoBinaryUsesExplicitPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mihomo")
	if err := os.WriteFile(path, []byte("fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := findMihomoBinary(path)
	if err != nil {
		t.Fatalf("findMihomoBinary: %v", err)
	}
	if got != path {
		t.Fatalf("findMihomoBinary() = %q, want %q", got, path)
	}
}
